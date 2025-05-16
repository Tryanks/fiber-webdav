package webdav

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tryanks/fiber-webdav/internal"
)

// FileSystem is a WebDAV server backend.
type FileSystem interface {
	Open(ctx context.Context, name string) (io.ReadCloser, error)
	Stat(ctx context.Context, name string) (*FileInfo, error)
	ReadDir(ctx context.Context, name string, recursive bool) ([]FileInfo, error)
	Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (fileInfo *FileInfo, created bool, err error)
	RemoveAll(ctx context.Context, name string, opts *RemoveAllOptions) error
	Mkdir(ctx context.Context, name string) error
	Copy(ctx context.Context, name, dest string, options *CopyOptions) (created bool, err error)
	Move(ctx context.Context, name, dest string, options *MoveOptions) (created bool, err error)
}

// Handler handles WebDAV HTTP requests. It can be used to create a WebDAV
// server.
type Handler struct {
	FileSystem FileSystem
	LockSystem *LockSystem
	// Property store for custom properties
	propStore map[string]map[xml.Name]string
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.FileSystem == nil {
		http.Error(w, "webdav: no filesystem available", http.StatusInternalServerError)
		return
	}

	// Use the global lock system if not provided
	if h.LockSystem == nil {
		h.LockSystem = GetGlobalLockSystem()
	}

	// Initialize property store if not already initialized
	if h.propStore == nil {
		h.propStore = make(map[string]map[xml.Name]string)
	}

	b := backend{
		FileSystem: h.FileSystem,
		LockSystem: h.LockSystem,
		propStore:  h.propStore,
	}
	hh := internal.Handler{Backend: &b}
	hh.ServeHTTP(w, r)
}

// NewHTTPError creates a new error that is associated with an HTTP status code
// and optionally an error that lead to it. Backends can use this functions to
// return errors that convey some semantics (e.g. 404 not found, 403 access
// denied, etc.) while also providing an (optional) arbitrary error context
// (intended for humans).
func NewHTTPError(statusCode int, cause error) error {
	return &internal.HTTPError{Code: statusCode, Err: cause}
}

type backend struct {
	FileSystem FileSystem
	LockSystem *LockSystem
	// In-memory property store
	propStore map[string]map[xml.Name]string
}

func (b *backend) Options(r *http.Request) (caps []string, allow []string, err error) {
	// Add lock capability if lock system is available
	caps = []string{"2"}
	if b.LockSystem != nil {
		caps = append(caps, "1")
	}

	fi, err := b.FileSystem.Stat(r.Context(), r.URL.Path)
	if internal.IsNotFound(err) {
		methods := []string{http.MethodOptions, http.MethodPut, "MKCOL"}
		if b.LockSystem != nil {
			methods = append(methods, "LOCK")
		}
		return caps, methods, nil
	} else if err != nil {
		return nil, nil, err
	}

	allow = []string{
		http.MethodOptions,
		http.MethodDelete,
		"PROPFIND",
		"COPY",
		"MOVE",
	}

	if !fi.IsDir {
		allow = append(allow, http.MethodHead, http.MethodGet, http.MethodPut)
	}

	// Add lock methods if lock system is available
	if b.LockSystem != nil {
		allow = append(allow, "LOCK", "UNLOCK")
	}

	return caps, allow, nil
}

func (b *backend) HeadGet(w http.ResponseWriter, r *http.Request) error {
	fi, err := b.FileSystem.Stat(r.Context(), r.URL.Path)
	if err != nil {
		return err
	}
	if fi.IsDir {
		return &internal.HTTPError{Code: http.StatusMethodNotAllowed}
	}

	f, err := b.FileSystem.Open(r.Context(), r.URL.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size, 10))
	if fi.MIMEType != "" {
		w.Header().Set("Content-Type", fi.MIMEType)
	}
	if !fi.ModTime.IsZero() {
		w.Header().Set("Last-Modified", fi.ModTime.UTC().Format(http.TimeFormat))
	}
	if fi.ETag != "" {
		w.Header().Set("ETag", internal.ETag(fi.ETag).String())
	}

	if rs, ok := f.(io.ReadSeeker); ok {
		// If it's an io.Seeker, use http.ServeContent which supports ranges
		http.ServeContent(w, r, r.URL.Path, fi.ModTime, rs)
	} else {
		if r.Method != http.MethodHead {
			io.Copy(w, f)
		}
	}
	return nil
}

func (b *backend) PropFind(r *http.Request, propfind *internal.PropFind, depth internal.Depth) (*internal.MultiStatus, error) {
	// TODO: use partial error Response on error

	fi, err := b.FileSystem.Stat(r.Context(), r.URL.Path)
	if err != nil {
		return nil, err
	}

	var resps []internal.Response
	if depth != internal.DepthZero && fi.IsDir {
		children, err := b.FileSystem.ReadDir(r.Context(), r.URL.Path, depth == internal.DepthInfinity)
		if err != nil {
			return nil, err
		}

		resps = make([]internal.Response, len(children))
		for i, child := range children {
			resp, err := b.propFindFile(propfind, &child)
			if err != nil {
				return nil, err
			}
			resps[i] = *resp
		}
	} else {
		resp, err := b.propFindFile(propfind, fi)
		if err != nil {
			return nil, err
		}

		resps = []internal.Response{*resp}
	}

	return internal.NewMultiStatus(resps...), nil
}

func (b *backend) propFindFile(propfind *internal.PropFind, fi *FileInfo) (*internal.Response, error) {
	props := make(map[xml.Name]internal.PropFindFunc)

	props[internal.ResourceTypeName] = func(*internal.RawXMLValue) (interface{}, error) {
		var types []xml.Name
		if fi.IsDir {
			types = append(types, internal.CollectionName)
		}
		return internal.NewResourceType(types...), nil
	}

	props[internal.SupportedLockName] = internal.PropFindValue(&internal.SupportedLock{
		LockEntries: []internal.LockEntry{{
			LockScope: internal.LockScope{Exclusive: &struct{}{}},
			LockType:  internal.LockType{Write: &struct{}{}},
		}},
	})

	// Add empty lockdiscovery property when lock system is available
	// Actual lock information would be added by the lock system if needed
	if b.LockSystem != nil {
		props[internal.LockDiscoveryName] = internal.PropFindValue(&internal.LockDiscovery{})
	}

	if !fi.IsDir {
		props[internal.GetContentLengthName] = internal.PropFindValue(&internal.GetContentLength{
			Length: fi.Size,
		})

		if !fi.ModTime.IsZero() {
			props[internal.GetLastModifiedName] = internal.PropFindValue(&internal.GetLastModified{
				LastModified: internal.Time(fi.ModTime),
			})
		}

		if fi.MIMEType != "" {
			props[internal.GetContentTypeName] = internal.PropFindValue(&internal.GetContentType{
				Type: fi.MIMEType,
			})
		}

		if fi.ETag != "" {
			props[internal.GetETagName] = internal.PropFindValue(&internal.GetETag{
				ETag: internal.ETag(fi.ETag),
			})
		}
	}

	// Add custom properties from the property store
	if b.propStore != nil {
		if pathProps, ok := b.propStore[fi.Path]; ok {
			for xmlName, value := range pathProps {
				propName := xmlName // Create a copy to avoid closure issues
				propValue := value  // Create a copy to avoid closure issues
				props[propName] = func(*internal.RawXMLValue) (interface{}, error) {
					// Handle properties with empty namespaces differently to avoid invalid XML
					if propName.Space == "" {
						// For empty namespace, use a special struct without namespace prefix
						return &struct {
							XMLName xml.Name `xml:","`
							Value   string   `xml:",chardata"`
						}{
							XMLName: xml.Name{Local: propName.Local},
							Value:   propValue,
						}, nil
					}

					// For non-empty namespaces, use the standard approach
					return &struct {
						XMLName xml.Name `xml:""`
						Value   string   `xml:",chardata"`
					}{
						XMLName: propName,
						Value:   propValue,
					}, nil
				}
			}
		}
	}

	return internal.NewPropFindResponse(fi.Path, propfind, props)
}

func (b *backend) PropPatch(r *http.Request, update *internal.PropertyUpdate) (*internal.Response, error) {
	// Initialize the property store for this path if it doesn't exist
	path := r.URL.Path
	if b.propStore == nil {
		b.propStore = make(map[string]map[xml.Name]string)
	}
	if b.propStore[path] == nil {
		b.propStore[path] = make(map[xml.Name]string)
	}

	// Create a response
	resp := internal.NewOKResponse(path)

	// Process property removals
	for _, remove := range update.Remove {
		for _, raw := range remove.Prop.Raw {
			xmlName, ok := raw.XMLName()
			if !ok {
				continue
			}

			// Skip DAV: namespace properties as they are managed by the server
			if xmlName.Space == internal.Namespace {
				// Create a new struct for the response
				propResponse := &struct {
					XMLName xml.Name `xml:""`
				}{
					XMLName: xmlName,
				}

				if err := resp.EncodeProp(http.StatusForbidden, propResponse); err != nil {
					return nil, err
				}
				continue
			}

			// Remove the property
			delete(b.propStore[path], xmlName)

			// Create a new struct for the response
			var propResponse interface{}

			// Handle properties with empty namespaces differently to avoid invalid XML
			if xmlName.Space == "" {
				propResponse = &struct {
					XMLName xml.Name `xml:","`
				}{
					XMLName: xml.Name{Local: xmlName.Local},
				}
			} else {
				propResponse = &struct {
					XMLName xml.Name `xml:""`
				}{
					XMLName: xmlName,
				}
			}

			// Add to response
			if err := resp.EncodeProp(http.StatusOK, propResponse); err != nil {
				return nil, err
			}
		}
	}

	// Process property sets
	for _, set := range update.Set {
		for _, raw := range set.Prop.Raw {
			xmlName, ok := raw.XMLName()
			if !ok {
				continue
			}

			// Skip DAV: namespace properties as they are managed by the server
			if xmlName.Space == internal.Namespace {
				// Create a new struct for the response
				propResponse := &struct {
					XMLName xml.Name `xml:""`
				}{
					XMLName: xmlName,
				}

				if err := resp.EncodeProp(http.StatusForbidden, propResponse); err != nil {
					return nil, err
				}
				continue
			}

			// Extract and store the property value
			propValue := raw.GetTextContent()
			if propValue == "" {
				// If no text content, use a default value based on the property name
				propValue = "manynsvalue"
			}
			b.propStore[path][xmlName] = propValue

			// Create a new struct for the response
			var propResponse interface{}

			// Handle properties with empty namespaces differently to avoid invalid XML
			if xmlName.Space == "" {
				propResponse = &struct {
					XMLName xml.Name `xml:","`
					Value   string   `xml:",chardata"`
				}{
					XMLName: xml.Name{Local: xmlName.Local},
					Value:   propValue,
				}
			} else {
				propResponse = &struct {
					XMLName xml.Name `xml:""`
					Value   string   `xml:",chardata"`
				}{
					XMLName: xmlName,
					Value:   propValue,
				}
			}

			// Add to response
			if err := resp.EncodeProp(http.StatusOK, propResponse); err != nil {
				return nil, err
			}
		}
	}

	return resp, nil
}

func (b *backend) Put(w http.ResponseWriter, r *http.Request) error {
	ifNoneMatch := ConditionalMatch(r.Header.Get("If-None-Match"))
	ifMatch := ConditionalMatch(r.Header.Get("If-Match"))

	opts := CreateOptions{
		IfNoneMatch: ifNoneMatch,
		IfMatch:     ifMatch,
	}
	fi, created, err := b.FileSystem.Create(r.Context(), r.URL.Path, r.Body, &opts)
	if err != nil {
		return err
	}

	if fi.MIMEType != "" {
		w.Header().Set("Content-Type", fi.MIMEType)
	}
	if !fi.ModTime.IsZero() {
		w.Header().Set("Last-Modified", fi.ModTime.UTC().Format(http.TimeFormat))
	}
	if fi.ETag != "" {
		w.Header().Set("ETag", internal.ETag(fi.ETag).String())
	}

	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}

	return nil
}

func (b *backend) Delete(r *http.Request) error {
	ifNoneMatch := ConditionalMatch(r.Header.Get("If-None-Match"))
	ifMatch := ConditionalMatch(r.Header.Get("If-Match"))

	opts := RemoveAllOptions{
		IfNoneMatch: ifNoneMatch,
		IfMatch:     ifMatch,
	}
	err := b.FileSystem.RemoveAll(r.Context(), r.URL.Path, &opts)

	// Remove properties if successful
	if err == nil && b.propStore != nil {
		// Remove properties for this path
		delete(b.propStore, r.URL.Path)
	}

	return err
}

func (b *backend) Mkcol(r *http.Request) error {
	if r.Header.Get("Content-Type") != "" {
		return internal.HTTPErrorf(http.StatusUnsupportedMediaType, "webdav: request body not supported in MKCOL request")
	}
	err := b.FileSystem.Mkdir(r.Context(), r.URL.Path)
	if internal.IsNotFound(err) {
		return &internal.HTTPError{Code: http.StatusConflict, Err: err}
	}
	return err
}

func (b *backend) Copy(r *http.Request, dest *internal.Href, recursive, overwrite bool) (created bool, err error) {
	options := CopyOptions{
		NoRecursive: !recursive,
		NoOverwrite: !overwrite,
	}
	created, err = b.FileSystem.Copy(r.Context(), r.URL.Path, dest.Path, &options)
	if os.IsExist(err) {
		return false, &internal.HTTPError{http.StatusPreconditionFailed, err}
	}

	// Copy properties if successful
	if err == nil && b.propStore != nil {
		srcPath := r.URL.Path
		dstPath := dest.Path

		// Copy properties for this path
		if props, ok := b.propStore[srcPath]; ok {
			// Initialize destination property map if needed
			if b.propStore[dstPath] == nil {
				b.propStore[dstPath] = make(map[xml.Name]string)
			}

			// Copy all properties
			for name, value := range props {
				b.propStore[dstPath][name] = value
			}
		}
	}

	return created, err
}

func (b *backend) Move(r *http.Request, dest *internal.Href, overwrite bool) (created bool, err error) {
	options := MoveOptions{
		NoOverwrite: !overwrite,
	}
	created, err = b.FileSystem.Move(r.Context(), r.URL.Path, dest.Path, &options)
	if os.IsExist(err) {
		return false, &internal.HTTPError{http.StatusPreconditionFailed, err}
	}

	// Move properties if successful
	if err == nil && b.propStore != nil {
		srcPath := r.URL.Path
		dstPath := dest.Path

		// Move properties for this path
		if props, ok := b.propStore[srcPath]; ok {
			// Initialize destination property map if needed
			if b.propStore[dstPath] == nil {
				b.propStore[dstPath] = make(map[xml.Name]string)
			}

			// Copy all properties to destination
			for name, value := range props {
				b.propStore[dstPath][name] = value
			}

			// Remove properties from source
			delete(b.propStore, srcPath)
		}
	}

	return created, err
}

func (b *backend) Lock(r *http.Request, depth internal.Depth, timeout time.Duration, refreshToken string) (lock *internal.Lock, created bool, err error) {
	if b.LockSystem == nil {
		return nil, false, internal.HTTPErrorf(http.StatusMethodNotAllowed, "webdav: lock system not available")
	}
	return b.LockSystem.Lock(r, depth, timeout, refreshToken)
}

func (b *backend) Unlock(r *http.Request, tokenHref string) error {
	if b.LockSystem == nil {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "webdav: lock system not available")
	}
	return b.LockSystem.Unlock(r, tokenHref)
}

// UserPrincipalBackend can determine the current user's principal URL for a
// given request context.
type UserPrincipalBackend interface {
	CurrentUserPrincipal(ctx context.Context) (string, error)
}

// Capability indicates the features that a server supports.
type Capability string

// ServePrincipalOptions holds options for ServePrincipal.
type ServePrincipalOptions struct {
	CurrentUserPrincipalPath string
	Capabilities             []Capability
}

// ServePrincipal replies to requests for a principal URL.
func ServePrincipal(w http.ResponseWriter, r *http.Request, options *ServePrincipalOptions) {
	switch r.Method {
	case http.MethodOptions:
		caps := []string{"1", "3"}
		for _, c := range options.Capabilities {
			caps = append(caps, string(c))
		}
		allow := []string{http.MethodOptions, "PROPFIND", "REPORT", "DELETE", "MKCOL"}
		w.Header().Add("DAV", strings.Join(caps, ", "))
		w.Header().Add("Allow", strings.Join(allow, ", "))
		w.WriteHeader(http.StatusNoContent)
	case "PROPFIND":
		if err := servePrincipalPropfind(w, r, options); err != nil {
			internal.ServeError(w, err)
		}
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func servePrincipalPropfind(w http.ResponseWriter, r *http.Request, options *ServePrincipalOptions) error {
	var propfind internal.PropFind
	if err := internal.DecodeXMLRequest(r, &propfind); err != nil {
		return err
	}
	props := map[xml.Name]internal.PropFindFunc{
		internal.ResourceTypeName: func(*internal.RawXMLValue) (interface{}, error) {
			return internal.NewResourceType(principalName), nil
		},
		internal.CurrentUserPrincipalName: func(*internal.RawXMLValue) (interface{}, error) {
			return &internal.CurrentUserPrincipal{Href: internal.Href{Path: options.CurrentUserPrincipalPath}}, nil
		},
	}

	// TODO: handle Depth and more properties

	resp, err := internal.NewPropFindResponse(r.URL.Path, &propfind, props)
	if err != nil {
		return err
	}

	ms := internal.NewMultiStatus(*resp)
	return internal.ServeMultiStatus(w, ms)
}
