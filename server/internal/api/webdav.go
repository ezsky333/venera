package api

import (
	"crypto/subtle"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WebDAV 兼容层（W1 兜底方案）。
//
// App 现有的 DataSync（lib/utils/data_sync.dart）就是一个极简 WebDAV 客户端，
// 只会用四个动作：readDir(PROPFIND)、write(PUT)、read2File(GET)、remove(DELETE)。
// 因此这里只需实现这四者的最小子集，即可让"未改造的 App"也能把同步包上传上来，
// 服务端再解析其中的 local_favorite.db / comic_source 数据。
//
// 这不是主方案（主方案是 W2 API），而是：
//   - 降低接入门槛：用户不必先升级 App
//   - 迁移通道：老用户的历史数据可以一次性导入
func (s *Server) webdavHandler() http.Handler {
	root := filepath.Join(s.cfg.DataDir, "webdav")
	_ = os.MkdirAll(root, 0o755)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebDAV 用 Basic Auth，与 API 的 Bearer Token 分离
		if s.cfg.WebDAVUser != "" || s.cfg.WebDAVPass != "" {
			user, pass, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.WebDAVUser)) != 1 ||
				subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.WebDAVPass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="venera-nas"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		rel := strings.TrimPrefix(r.URL.Path, "/webdav")
		rel = strings.TrimPrefix(rel, "/")
		clean := filepath.Clean("/" + rel)
		full := filepath.Join(root, filepath.FromSlash(clean))

		// 路径穿越防护
		absRoot, _ := filepath.Abs(root)
		absFull, _ := filepath.Abs(full)
		if absFull != absRoot && !strings.HasPrefix(absFull, absRoot+string(os.PathSeparator)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		switch r.Method {
		case "PROPFIND":
			s.webdavPropfind(w, r, absFull, rel)
		case "GET":
			http.ServeFile(w, r, absFull)
		case http.MethodPut:
			s.webdavPut(w, r, absFull)
		case http.MethodDelete:
			if err := os.RemoveAll(absFull); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodOptions:
			w.Header().Set("DAV", "1, 2")
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND")
			w.WriteHeader(http.StatusOK)
		case "MKCOL":
			if err := os.MkdirAll(absFull, 0o755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (s *Server) webdavPut(w http.ResponseWriter, r *http.Request, full string) {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Create(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(f, io.LimitReader(r.Body, 1<<30)); err != nil {
		f.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.Close()
	w.WriteHeader(http.StatusCreated)

	// 异步导入：把 .venera 同步包解出来，同步到服务端状态
	if strings.HasSuffix(strings.ToLower(full), ".venera") {
		go func() {
			if err := s.ImportVeneraArchive(full); err != nil {
				s.log.Error("import venera archive failed", "file", full, "err", err)
			} else {
				s.log.Info("imported venera archive", "file", filepath.Base(full))
			}
		}()
	}
}

// webdavPropfind 返回目录/文件列表，格式与标准 WebDAV 一致。
func (s *Server) webdavPropfind(w http.ResponseWriter, r *http.Request, full, rel string) {
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	type propstat struct {
		XMLName xml.Name   `xml:"d:propstat"`
		Prop    propFields `xml:"d:prop"`
		Status  string     `xml:"d:status"`
	}
	type response struct {
		XMLName    xml.Name   `xml:"d:response"`
		Href       string     `xml:"d:href"`
		Propstat   []propstat `xml:"d:propstat"`
	}
	type multistatus struct {
		XMLName   xml.Name   `xml:"d:multistatus"`
		XmlnsD    string     `xml:"xmlns:d,attr"`
		Responses []response `xml:"d:response"`
	}

	makeResp := func(name string, isDir bool, size int64, mod time.Time) response {
		href := "/webdav/" + strings.TrimPrefix(filepath.ToSlash(name), "/")
		if isDir && !strings.HasSuffix(href, "/") {
			href += "/"
		}
		resType := ""
		if isDir {
			resType = "<d:collection/>"
		}
		return response{
			Href: href,
			Propstat: []propstat{{
				Prop: propFields{
					DisplayName: filepath.Base(name),
					ContentLength: func() int64 {
						if isDir {
							return 0
						}
						return size
					}(),
					LastModified: mod.UTC().Format(http.TimeFormat),
					ResourceType: resType,
				},
				Status: "HTTP/1.1 200 OK",
			}},
		}
	}

	ms := multistatus{XmlnsD: "DAV:"}
	if info.IsDir() {
		ms.Responses = append(ms.Responses, makeResp(rel, true, 0, info.ModTime()))
		entries, err := os.ReadDir(full)
		if err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				fi, err := os.Stat(filepath.Join(full, name))
				if err != nil {
					continue
				}
				sub := filepath.ToSlash(filepath.Join(rel, name))
				ms.Responses = append(ms.Responses, makeResp(sub, fi.IsDir(), fi.Size(), fi.ModTime()))
			}
		}
	} else {
		ms.Responses = append(ms.Responses, makeResp(rel, false, info.Size(), info.ModTime()))
	}

	body, err := xml.Marshal(ms)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := append([]byte(xml.Header), body...)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(207) // Multi-Status
	_, _ = w.Write(out)
}

// propFields 是 PROPFIND 响应里的属性集合。
type propFields struct {
	XMLName       xml.Name `xml:"d:prop"`
	DisplayName   string   `xml:"d:displayname"`
	ContentLength int64    `xml:"d:getcontentlength"`
	LastModified  string   `xml:"d:getlastmodified"`
	ResourceType  string   `xml:"d:resourcetype"`
}

var _ = fmt.Sprintf
