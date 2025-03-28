// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package webdav provides a WebDAV server implementation.
package webdav // import "golang.org/x/net/webdav"

import (
	"errors"
	"fmt"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type Handler struct {
	// Prefix is the URL path prefix to strip from WebDAV resource paths.
	Prefix string
	// FileSystem is the virtual file system.
	FileSystem FileSystem
	// LockSystem is the lock management system.
	LockSystem LockSystem
	// Logger is an optional error logger. If non-nil, it will be called
	// for all HTTP requests.
	Logger func(int, error)
}

func NewWebdavServer(prefix string, fs FileSystem, ls LockSystem) *Handler {
	return &Handler{
		Prefix:     prefix,
		FileSystem: fs,
		LockSystem: ls,
	}
}

func (h *Handler) stripPrefix(p string) (string, int, error) {
	if h.Prefix == "" {
		return p, fiber.StatusOK, nil
	}
	if r := strings.TrimPrefix(p, h.Prefix); len(r) < len(p) {
		return r, fiber.StatusOK, nil
	}
	return p, fiber.StatusNotFound, errPrefixMismatch
}

func (h *Handler) ServeFiber(c fiber.Ctx) error {
	// Fix sardine HEAD with Folder
	// Reference: https://github.com/hacdias/webdav/blob/f0b5a39d802a4db0245f225a3f68d71efd866f1b/lib/handler.go#L143
	if (c.Method() == fiber.MethodGet || c.Method() == fiber.MethodHead) &&
		strings.HasSuffix(c.Path(), h.Prefix) {
		info, err := h.FileSystem.Stat(c.RequestCtx(), strings.TrimPrefix(c.Path(), h.Prefix))
		if err == nil && info.IsDir() {
			c.Method(MethodPropfind)

			if c.Get("Depth") == "" {
				c.Set("Depth", "1")
			}
		}
	}

	handler := adaptor.HTTPHandler(h)
	return handler(c)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, err := fiber.StatusBadRequest, errUnsupportedMethod
	if h.FileSystem == nil {
		status, err = fiber.StatusInternalServerError, errNoFileSystem
	} else if h.LockSystem == nil {
		status, err = fiber.StatusInternalServerError, errNoLockSystem
	} else {
		switch r.Method {
		case fiber.MethodOptions:
			status, err = h.handleOptions(w, r)
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodPost:
			status, err = h.handleGetHeadPost(w, r)
		case fiber.MethodDelete:
			status, err = h.handleDelete(w, r)
		case fiber.MethodPut:
			status, err = h.handlePut(w, r)
		case MethodMkcol:
			status, err = h.handleMkcol(w, r)
		case MethodCopy, MethodMove:
			status, err = h.handleCopyMove(w, r)
		case MethodLock:
			status, err = h.handleLock(w, r)
		case MethodUnlock:
			status, err = h.handleUnlock(w, r)
		case MethodPropfind:
			status, err = h.handlePropfind(w, r)
		case MethodProppatch:
			status, err = h.handleProppatch(w, r)
		}
	}

	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(StatusText(status)))
	}

	if err != nil && h.Logger != nil {
		h.Logger(status, err)
	}
}

func (h *Handler) lock(now time.Time, root string) (token string, status int, err error) {
	token, err = h.LockSystem.Create(now, LockDetails{
		Root:      root,
		Duration:  infiniteTimeout,
		ZeroDepth: true,
	})
	if err != nil {
		if err == ErrLocked {
			return "", StatusLocked, err
		}
		return "", fiber.StatusInternalServerError, err
	}
	return token, 0, nil
}

func (h *Handler) confirmLocks(r *http.Request, src, dst string) (release func(), status int, err error) {
	hdr := r.Header.Get("If")
	if hdr == "" {
		// An empty If header means that the client hasn't previously created locks.
		// Even if this client doesn't care about locks, we still need to check that
		// the resources aren't locked by another client, so we create temporary
		// locks that would conflict with another client's locks. These temporary
		// locks are unlocked at the end of the HTTP request.
		now, srcToken, dstToken := time.Now(), "", ""
		if src != "" {
			srcToken, status, err = h.lock(now, src)
			if err != nil {
				return nil, status, err
			}
		}
		if dst != "" {
			dstToken, status, err = h.lock(now, dst)
			if err != nil {
				if srcToken != "" {
					h.LockSystem.Unlock(now, srcToken)
				}
				return nil, status, err
			}
		}

		return func() {
			if dstToken != "" {
				h.LockSystem.Unlock(now, dstToken)
			}
			if srcToken != "" {
				h.LockSystem.Unlock(now, srcToken)
			}
		}, 0, nil
	}

	ih, ok := parseIfHeader(hdr)
	if !ok {
		return nil, fiber.StatusBadRequest, errInvalidIfHeader
	}
	// ih is a disjunction (OR) of ifLists, so any ifList will do.
	for _, l := range ih.lists {
		lsrc := l.resourceTag
		if lsrc == "" {
			lsrc = src
		} else {
			u, err := url.Parse(lsrc)
			if err != nil {
				continue
			}
			if u.Host != r.Host {
				continue
			}
			lsrc, status, err = h.stripPrefix(u.Path)
			if err != nil {
				return nil, status, err
			}
		}
		release, err = h.LockSystem.Confirm(time.Now(), lsrc, dst, l.conditions...)
		if err == ErrConfirmationFailed {
			continue
		}
		if err != nil {
			return nil, fiber.StatusInternalServerError, err
		}
		return release, 0, nil
	}
	// Section 10.4.1 says that "If this header is evaluated and all state lists
	// fail, then the request must fail with a 412 (Precondition Failed) status."
	// We follow the spec even though the cond_put_corrupt_token test case from
	// the litmus test warns on seeing a 412 instead of a 423 (Locked).
	return nil, fiber.StatusPreconditionFailed, ErrLocked
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := r.Context()
	allow := "OPTIONS, LOCK, PUT, MKCOL"
	if fi, err := h.FileSystem.Stat(ctx, reqPath); err == nil {
		if fi.IsDir() {
			allow = "OPTIONS, LOCK, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND"
		} else {
			allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND, PUT"
		}
	}
	w.Header().Set("Allow", allow)
	// http://www.webdav.org/specs/rfc4918.html#dav.compliance.classes
	w.Header().Set("DAV", "1, 2")
	// http://msdn.microsoft.com/en-au/library/cc250217.aspx
	w.Header().Set("MS-Author-Via", "DAV")
	return 0, nil
}

func (h *Handler) handleGetHeadPost(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	// TODO: check locks for read-only access??
	ctx := r.Context()
	f, err := h.FileSystem.OpenFile(ctx, reqPath, os.O_RDONLY, 0)
	if err != nil {
		return fiber.StatusNotFound, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fiber.StatusNotFound, err
	}
	if fi.IsDir() {
		return fiber.StatusMethodNotAllowed, nil
	}
	etag, err := findETag(ctx, h.FileSystem, h.LockSystem, reqPath, fi)
	if err != nil {
		return fiber.StatusInternalServerError, err
	}
	w.Header().Set("ETag", etag)
	// Let ServeContent determine the Content-Type header.
	http.ServeContent(w, r, reqPath, fi.ModTime(), f)
	return 0, nil
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	// TODO: return MultiStatus where appropriate.

	// "godoc os RemoveAll" says that "If the path does not exist, RemoveAll
	// returns nil (no error)." WebDAV semantics are that it should return a
	// "404 Not Found". We therefore have to Stat before we RemoveAll.
	if _, err := h.FileSystem.Stat(ctx, reqPath); err != nil {
		if os.IsNotExist(err) {
			return fiber.StatusNotFound, err
		}
		return fiber.StatusMethodNotAllowed, err
	}
	if err := h.FileSystem.RemoveAll(ctx, reqPath); err != nil {
		return fiber.StatusMethodNotAllowed, err
	}
	return fiber.StatusNoContent, nil
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()
	// TODO(rost): Support the If-Match, If-None-Match headers? See bradfitz'
	// comments in http.checkEtag.
	ctx := r.Context()

	f, err := h.FileSystem.OpenFile(ctx, reqPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		if os.IsNotExist(err) {
			return fiber.StatusConflict, err
		}
		return fiber.StatusNotFound, err
	}
	_, copyErr := io.Copy(f, r.Body)
	fi, statErr := f.Stat()
	closeErr := f.Close()
	// TODO(rost): Returning 405 Method Not Allowed might not be appropriate.
	if copyErr != nil {
		return fiber.StatusMethodNotAllowed, copyErr
	}
	if statErr != nil {
		return fiber.StatusMethodNotAllowed, statErr
	}
	if closeErr != nil {
		return fiber.StatusMethodNotAllowed, closeErr
	}
	etag, err := findETag(ctx, h.FileSystem, h.LockSystem, reqPath, fi)
	if err != nil {
		return fiber.StatusInternalServerError, err
	}
	w.Header().Set("ETag", etag)
	return fiber.StatusCreated, nil
}

func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	if r.ContentLength > 0 {
		return fiber.StatusUnsupportedMediaType, nil
	}
	if err := h.FileSystem.Mkdir(ctx, reqPath, 0777); err != nil {
		if os.IsNotExist(err) {
			return fiber.StatusConflict, err
		}
		return fiber.StatusMethodNotAllowed, err
	}
	return fiber.StatusCreated, nil
}

func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request) (status int, err error) {
	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return fiber.StatusBadRequest, errInvalidDestination
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return fiber.StatusBadRequest, errInvalidDestination
	}
	if u.Host != "" && u.Host != r.Host {
		return fiber.StatusBadGateway, errInvalidDestination
	}

	src, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}

	dst, status, err := h.stripPrefix(u.Path)
	if err != nil {
		return status, err
	}

	if dst == "" {
		return fiber.StatusBadGateway, errInvalidDestination
	}
	if dst == src {
		return fiber.StatusForbidden, errDestinationEqualsSource
	}

	ctx := r.Context()

	if r.Method == "COPY" {
		// Section 7.5.1 says that a COPY only needs to lock the destination,
		// not both destination and source. Strictly speaking, this is racy,
		// even though a COPY doesn't modify the source, if a concurrent
		// operation modifies the source. However, the litmus test explicitly
		// checks that COPYing a locked-by-another source is OK.
		release, status, err := h.confirmLocks(r, "", dst)
		if err != nil {
			return status, err
		}
		defer release()

		// Section 9.8.3 says that "The COPY method on a collection without a Depth
		// header must act as if a Depth header with value "infinity" was included".
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.8.3 says that "A client may submit a Depth header on a
				// COPY on a collection with a value of "0" or "infinity"."
				return fiber.StatusBadRequest, errInvalidDepth
			}
		}
		return copyFiles(ctx, h.FileSystem, src, dst, r.Header.Get("Overwrite") != "F", depth, 0)
	}

	release, status, err := h.confirmLocks(r, src, dst)
	if err != nil {
		return status, err
	}
	defer release()

	// Section 9.9.2 says that "The MOVE method on a collection must act as if
	// a "Depth: infinity" header was used on it. A client must not submit a
	// Depth header on a MOVE on a collection with any value but "infinity"."
	if hdr := r.Header.Get("Depth"); hdr != "" {
		if parseDepth(hdr) != infiniteDepth {
			return fiber.StatusBadRequest, errInvalidDepth
		}
	}
	return moveFiles(ctx, h.FileSystem, src, dst, r.Header.Get("Overwrite") == "T")
}

func (h *Handler) handleLock(w http.ResponseWriter, r *http.Request) (retStatus int, retErr error) {
	duration, err := parseTimeout(r.Header.Get("Timeout"))
	if err != nil {
		return fiber.StatusBadRequest, err
	}
	li, status, err := readLockInfo(r.Body)
	if err != nil {
		return status, err
	}

	ctx := r.Context()
	token, ld, now, created := "", LockDetails{}, time.Now(), false
	if li == (lockInfo{}) {
		// An empty lockInfo means to refresh the lock.
		ih, ok := parseIfHeader(r.Header.Get("If"))
		if !ok {
			return fiber.StatusBadRequest, errInvalidIfHeader
		}
		if len(ih.lists) == 1 && len(ih.lists[0].conditions) == 1 {
			token = ih.lists[0].conditions[0].Token
		}
		if token == "" {
			return fiber.StatusBadRequest, errInvalidLockToken
		}
		ld, err = h.LockSystem.Refresh(now, token, duration)
		if err != nil {
			if err == ErrNoSuchLock {
				return fiber.StatusPreconditionFailed, err
			}
			return fiber.StatusInternalServerError, err
		}

	} else {
		// Section 9.10.3 says that "If no Depth header is submitted on a LOCK request,
		// then the request MUST act as if a "Depth:infinity" had been submitted."
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.10.3 says that "Values other than 0 or infinity must not be
				// used with the Depth header on a LOCK method".
				return fiber.StatusBadRequest, errInvalidDepth
			}
		}
		reqPath, status, err := h.stripPrefix(r.URL.Path)
		if err != nil {
			return status, err
		}
		ld = LockDetails{
			Root:      reqPath,
			Duration:  duration,
			OwnerXML:  li.Owner.InnerXML,
			ZeroDepth: depth == 0,
		}
		token, err = h.LockSystem.Create(now, ld)
		if err != nil {
			if err == ErrLocked {
				return StatusLocked, err
			}
			return fiber.StatusInternalServerError, err
		}
		defer func() {
			if retErr != nil {
				h.LockSystem.Unlock(now, token)
			}
		}()

		// Create the resource if it didn't previously exist.
		if _, err := h.FileSystem.Stat(ctx, reqPath); err != nil {
			f, err := h.FileSystem.OpenFile(ctx, reqPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
			if err != nil {
				// TODO: detect missing intermediate dirs and return fiber.StatusConflict?
				return fiber.StatusInternalServerError, err
			}
			f.Close()
			created = true
		}

		// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
		// Lock-Token value is a Coded-URL. We add angle brackets.
		w.Header().Set("Lock-Token", "<"+token+">")
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if created {
		// This is "w.WriteHeader(fiber.StatusCreated)" and not "return
		// fiber.StatusCreated, nil" because we write our own (XML) response to w
		// and Handler.ServeHTTP would otherwise write "Created".
		w.WriteHeader(fiber.StatusCreated)
	}
	writeLockInfo(w, token, ld)
	return 0, nil
}

func (h *Handler) handleUnlock(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
	// Lock-Token value is a Coded-URL. We strip its angle brackets.
	t := r.Header.Get("Lock-Token")
	if len(t) < 2 || t[0] != '<' || t[len(t)-1] != '>' {
		return fiber.StatusBadRequest, errInvalidLockToken
	}
	t = t[1 : len(t)-1]

	switch err = h.LockSystem.Unlock(time.Now(), t); err {
	case nil:
		return fiber.StatusNoContent, err
	case ErrForbidden:
		return fiber.StatusForbidden, err
	case ErrLocked:
		return StatusLocked, err
	case ErrNoSuchLock:
		return fiber.StatusConflict, err
	default:
		return fiber.StatusInternalServerError, err
	}
}

func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := r.Context()
	fi, err := h.FileSystem.Stat(ctx, reqPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fiber.StatusNotFound, err
		}
		return fiber.StatusMethodNotAllowed, err
	}
	depth := infiniteDepth
	if hdr := r.Header.Get("Depth"); hdr != "" {
		depth = parseDepth(hdr)
		if depth == invalidDepth {
			return fiber.StatusBadRequest, errInvalidDepth
		}
	}
	pf, status, err := readPropfind(r.Body)
	if err != nil {
		return status, err
	}

	mw := multistatusWriter{w: w}

	walkFn := func(reqPath string, info os.FileInfo, err error) error {
		if err != nil {
			return handlePropfindError(err, info)
		}

		var pstats []Propstat
		if pf.Propname != nil {
			pnames, err := propnames(ctx, h.FileSystem, h.LockSystem, reqPath)
			if err != nil {
				return handlePropfindError(err, info)
			}
			pstat := Propstat{Status: fiber.StatusOK}
			for _, xmlname := range pnames {
				pstat.Props = append(pstat.Props, Property{XMLName: xmlname})
			}
			pstats = append(pstats, pstat)
		} else if pf.Allprop != nil {
			pstats, err = allprop(ctx, h.FileSystem, h.LockSystem, reqPath, pf.Prop)
		} else {
			pstats, err = props(ctx, h.FileSystem, h.LockSystem, reqPath, pf.Prop)
		}
		if err != nil {
			return handlePropfindError(err, info)
		}
		href := path.Join(h.Prefix, reqPath)
		if href != "/" && info.IsDir() {
			href += "/"
		}
		return mw.write(makePropstatResponse(href, pstats))
	}

	walkErr := walkFS(ctx, h.FileSystem, depth, reqPath, fi, walkFn)
	closeErr := mw.close()
	if walkErr != nil {
		return fiber.StatusInternalServerError, walkErr
	}
	if closeErr != nil {
		return fiber.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	if _, err := h.FileSystem.Stat(ctx, reqPath); err != nil {
		if os.IsNotExist(err) {
			return fiber.StatusNotFound, err
		}
		return fiber.StatusMethodNotAllowed, err
	}
	patches, status, err := readProppatch(r.Body)
	if err != nil {
		return status, err
	}
	pstats, err := patch(ctx, h.FileSystem, h.LockSystem, reqPath, patches)
	if err != nil {
		return fiber.StatusInternalServerError, err
	}
	mw := multistatusWriter{w: w}
	writeErr := mw.write(makePropstatResponse(r.URL.Path, pstats))
	closeErr := mw.close()
	if writeErr != nil {
		return fiber.StatusInternalServerError, writeErr
	}
	if closeErr != nil {
		return fiber.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func makePropstatResponse(href string, pstats []Propstat) *response {
	resp := response{
		Href:     []string{(&url.URL{Path: href}).EscapedPath()},
		Propstat: make([]propstat, 0, len(pstats)),
	}
	for _, p := range pstats {
		var xmlErr *xmlError
		if p.XMLError != "" {
			xmlErr = &xmlError{InnerXML: []byte(p.XMLError)}
		}
		resp.Propstat = append(resp.Propstat, propstat{
			Status:              fmt.Sprintf("HTTP/1.1 %d %s", p.Status, StatusText(p.Status)),
			Prop:                p.Props,
			ResponseDescription: p.ResponseDescription,
			Error:               xmlErr,
		})
	}
	return &resp
}

func handlePropfindError(err error, info os.FileInfo) error {
	var skipResp error = nil
	if info != nil && info.IsDir() {
		skipResp = filepath.SkipDir
	}

	if errors.Is(err, os.ErrPermission) {
		// If the server cannot recurse into a directory because it is not allowed,
		// then there is nothing more to say about it. Just skip sending anything.
		return skipResp
	}

	if _, ok := err.(*os.PathError); ok {
		// If the file is just bad, it couldn't be a proper WebDAV resource. Skip it.
		return skipResp
	}

	// We need to be careful with other errors: there is no way to abort the xml stream
	// part way through while returning a valid PROPFIND response. Returning only half
	// the data would be misleading, but so would be returning results tainted by errors.
	// The current behaviour by returning an error here leads to the stream being aborted,
	// and the parent http server complaining about writing a spurious header. We should
	// consider further enhancing this error handling to more gracefully fail, or perhaps
	// buffer the entire response until we've walked the tree.
	return err
}

const (
	infiniteDepth = -1
	invalidDepth  = -2
)

// parseDepth maps the strings "0", "1" and "infinity" to 0, 1 and
// infiniteDepth. Parsing any other string returns invalidDepth.
//
// Different WebDAV methods have further constraints on valid depths:
//   - PROPFIND has no further restrictions, as per section 9.1.
//   - COPY accepts only "0" or "infinity", as per section 9.8.3.
//   - MOVE accepts only "infinity", as per section 9.9.2.
//   - LOCK accepts only "0" or "infinity", as per section 9.10.3.
//
// These constraints are enforced by the handleXxx methods.
func parseDepth(s string) int {
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "infinity":
		return infiniteDepth
	}
	return invalidDepth
}

// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
const (
	StatusMulti               = 207
	StatusUnprocessableEntity = 422
	StatusLocked              = 423
	StatusFailedDependency    = 424
	StatusInsufficientStorage = 507
)

func StatusText(code int) string {
	switch code {
	case StatusMulti:
		return "Multi-Status"
	case StatusUnprocessableEntity:
		return "Unprocessable Entity"
	case StatusLocked:
		return "Locked"
	case StatusFailedDependency:
		return "Failed Dependency"
	case StatusInsufficientStorage:
		return "Insufficient Storage"
	case fiber.StatusNoContent:
		return ""
	}
	return http.StatusText(code)
}

var (
	errDestinationEqualsSource = errors.New("webdav: destination equals source")
	errDirectoryNotEmpty       = errors.New("webdav: directory not empty")
	errInvalidDepth            = errors.New("webdav: invalid depth")
	errInvalidDestination      = errors.New("webdav: invalid destination")
	errInvalidIfHeader         = errors.New("webdav: invalid If header")
	errInvalidLockInfo         = errors.New("webdav: invalid lock info")
	errInvalidLockToken        = errors.New("webdav: invalid lock token")
	errInvalidPropfind         = errors.New("webdav: invalid propfind")
	errInvalidProppatch        = errors.New("webdav: invalid proppatch")
	errInvalidResponse         = errors.New("webdav: invalid response")
	errInvalidTimeout          = errors.New("webdav: invalid timeout")
	errNoFileSystem            = errors.New("webdav: no file system")
	errNoLockSystem            = errors.New("webdav: no lock system")
	errNotADirectory           = errors.New("webdav: not a directory")
	errPrefixMismatch          = errors.New("webdav: prefix mismatch")
	errRecursionTooDeep        = errors.New("webdav: recursion too deep")
	errUnsupportedLockInfo     = errors.New("webdav: unsupported lock info")
	errUnsupportedMethod       = errors.New("webdav: unsupported method")
)
