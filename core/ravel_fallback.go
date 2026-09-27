package core

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

func newRavelFallback(rawURL string) (http.Handler, error) {
	origin, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if origin.Opaque != "" || origin.Fragment != "" || origin.User != nil {
		return nil, errors.New("opaque data, fragment, and userinfo are not allowed")
	}
	switch origin.Scheme {
	case "file":
		root, err := ravelFileURLPath(origin)
		if err != nil {
			return nil, err
		}
		return newRavelSafeFileHandler(root)
	case "http", "https":
		if origin.Host == "" {
			return nil, errors.New("http(s) masquerade URL requires an origin host")
		}
		proxy := httputil.NewSingleHostReverseProxy(origin)
		director := proxy.Director
		proxy.Director = func(request *http.Request) {
			director(request)
			request.Host = origin.Host
			request.Header.Del("Forwarded")
			request.Header.Del("X-Forwarded-Host")
			request.Header.Del("X-Forwarded-Proto")
			request.Header["X-Forwarded-For"] = nil
		}
		proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_ = http.NewResponseController(writer).EnableFullDuplex()
			proxy.ServeHTTP(writer, request)
		}), nil
	default:
		return nil, errors.New("masquerade URL scheme must be file, http, or https")
	}
}

func ravelFileURLPath(origin *url.URL) (string, error) {
	if origin.Host != "" && origin.Host != "localhost" {
		return "", errors.New("file masquerade URL host must be empty or localhost")
	}
	if origin.RawQuery != "" {
		return "", errors.New("file masquerade URL must not contain a query")
	}
	decoded, err := url.PathUnescape(origin.EscapedPath())
	if err != nil {
		return "", fmt.Errorf("invalid file URL path: %w", err)
	}
	if runtime.GOOS == "windows" &&
		len(decoded) >= 3 &&
		decoded[0] == '/' &&
		decoded[2] == ':' {
		decoded = decoded[1:]
	}
	if decoded == "" {
		return "", errors.New("file masquerade URL requires an absolute directory")
	}
	root := filepath.FromSlash(decoded)
	if !filepath.IsAbs(root) {
		return "", errors.New("file masquerade URL requires an absolute directory")
	}
	return root, nil
}

type ravelSafeFileHandler struct {
	root string
}

func newRavelSafeFileHandler(root string) (http.Handler, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve file masquerade root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("file masquerade root must be a directory")
	}
	return &ravelSafeFileHandler{root: filepath.Clean(resolved)}, nil
}

func (handler *ravelSafeFileHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	cleanURLPath := path.Clean("/" + request.URL.Path)
	relative := strings.TrimPrefix(cleanURLPath, "/")
	candidate := filepath.Join(handler.root, filepath.FromSlash(relative))
	resolved, info, err := handler.resolveRegularFile(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(writer, request)
			return
		}
		http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if info.IsDir() {
		resolved, info, err = handler.resolveRegularFile(filepath.Join(resolved, "index.html"))
		if err != nil || info.IsDir() {
			http.NotFound(writer, request)
			return
		}
	}

	file, err := os.Open(resolved)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()
	if contentType := mime.TypeByExtension(filepath.Ext(resolved)); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(writer, request, info.Name(), info.ModTime(), file)
}

func (handler *ravelSafeFileHandler) resolveRegularFile(candidate string) (string, fs.FileInfo, error) {
	if !ravelPathWithinRoot(handler.root, candidate) {
		return "", nil, fs.ErrPermission
	}
	hasSymlink, err := ravelPathHasSymlink(handler.root, candidate)
	if err != nil {
		return "", nil, err
	}
	if hasSymlink {
		return "", nil, fs.ErrPermission
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", nil, err
	}
	if !ravelPathWithinRoot(handler.root, resolved) {
		return "", nil, fs.ErrPermission
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, err
	}
	return resolved, info, nil
}

func ravelPathHasSymlink(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

func ravelPathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
