package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The Remote storage tab of Backup Manager (backup_remote.go): the bucket settings, a connection
// test, and the encrypted backups in the bucket with Restore and Delete. Secret fields are never
// written back into the page: an empty field keeps the saved value.

type bmgrRemoteView struct {
	Endpoint, Region, Bucket, Prefix, AccessKey string
	PathStyle, Auto                             bool
	Keep                                        int
	SecretSet, PassSet                          bool
	Locked                                      map[string]string // field → environment variable
	Configured                                  bool
	Problem                                     string
	AllowPrivate                                bool
	Objects                                     []bmgrRemoteObjView
	ListErr                                     string
	MinPass                                     int
}

type bmgrRemoteObjView struct {
	Name, SizeText, When string
	Local                bool // already in the backups folder
}

func (s *Server) bmgrRemoteView(ctx context.Context) *bmgrRemoteView {
	st := s.bmgrRemote()
	v := &bmgrRemoteView{Endpoint: st.Endpoint, Region: st.Region, Bucket: st.Bucket, Prefix: st.Prefix, AccessKey: st.AccessKey,
		PathStyle: st.PathStyle, Auto: st.Auto, Keep: st.Keep, SecretSet: st.SecretKey != "", PassSet: st.Passphrase != "",
		Locked: bmgrRemoteLocked(), Configured: st.configured(), AllowPrivate: bmgrRemoteAllowPrivate(), MinPass: bmgrRemoteMinPass}
	if !v.Configured {
		return v
	}
	if err := st.problem(); err != nil {
		v.Problem = err.Error()
		return v
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	list, err := bmgrRemoteList(lctx, bmgrRemoteClient(st), st)
	if err != nil {
		v.ListErr = err.Error()
		return v
	}
	for _, o := range list {
		v.Objects = append(v.Objects, bmgrRemoteObjView{Name: o.Name, SizeText: bmgrHuman(o.Size),
			When: o.Modified.In(s.loc()).Format("2006-01-02 15:04"), Local: s.bmgrPath(o.LocalName()) != ""})
	}
	return v
}

func (s *Server) bmgrRemoteBack(extra string) string { return s.bmgrBack("&tab=remote" + extra) }

func bmgrErrParam(err error) string { return "&bmgr-error=" + url.QueryEscape(err.Error()) }

// bmgrPostRemote handles the Remote storage tab's forms (do=remote-*).
func (s *Server) bmgrPostRemote(c *gin.Context, do string) {
	ctx := c.Request.Context()
	switch do {
	case "remote-save":
		keep, err := 0, error(nil)
		if k := strings.TrimSpace(c.PostForm("keep")); k != "" {
			if keep, err = strconv.Atoi(k); err != nil {
				keep = -1
			}
		}
		in := bmgrRemoteSettings{Endpoint: c.PostForm("endpoint"), Region: c.PostForm("region"), Bucket: c.PostForm("bucket"),
			Prefix: c.PostForm("prefix"), AccessKey: c.PostForm("access_key"), SecretKey: c.PostForm("secret_key"),
			PathStyle: c.PostForm("path_style") == "1", Passphrase: c.PostForm("passphrase"), Auto: c.PostForm("auto") == "1", Keep: keep}
		if in.Passphrase != "" && in.Passphrase != c.PostForm("passphrase2") {
			c.Redirect(http.StatusFound, s.bmgrRemoteBack("&bmgr-error="+url.QueryEscape("The two passphrases do not match.")))
			return
		}
		if err := s.bmgrRemoteSave(ctx, in, c.PostForm("clear") == "1"); err != nil {
			c.Redirect(http.StatusFound, s.bmgrRemoteBack(bmgrErrParam(err)))
			return
		}
		c.Redirect(http.StatusFound, s.bmgrRemoteBack("&bmgr-msg=remote-saved"))
	case "remote-test":
		if _, err := s.bmgrRemoteTest(ctx); err != nil {
			c.Redirect(http.StatusFound, s.bmgrRemoteBack(bmgrErrParam(err)))
			return
		}
		c.Redirect(http.StatusFound, s.bmgrRemoteBack("&bmgr-msg=remote-ok"))
	case "remote-object": // a button of the bucket list: fetch=<name> or delete=<name>
		if name := c.PostForm("delete"); name != "" {
			s.bmgrPostRemote(c, "remote-delete")
			return
		}
		s.bmgrPostRemote(c, "remote-fetch")
	case "remote-delete":
		if err := s.bmgrRemoteDelete(ctx, c.PostForm("delete")); err != nil {
			c.Redirect(http.StatusFound, s.bmgrRemoteBack(bmgrErrParam(err)))
			return
		}
		c.Redirect(http.StatusFound, s.bmgrRemoteBack("&bmgr-msg=remote-deleted"))
	case "remote-upload":
		file := c.PostForm("file")
		if err := s.bmgrRemote().problem(); err != nil {
			c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups"+bmgrErrParam(err)))
			return
		}
		if s.bmgrPath(file) == "" {
			c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups&bmgr-error="+url.QueryEscape("The backup file was not found.")))
			return
		}
		j, err := s.bmgrStart("upload")
		if err != nil {
			c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups"+bmgrErrParam(err)))
			return
		}
		go func() {
			jctx := s.bmgrCancelable(j)
			s.bmgrUpdate(j, func(j *bmgrJob) { j.File = file })
			key, err := s.bmgrRemoteUpload(jctx, file, func(f string, a ...any) { s.bmgrLog(j, f, a...) },
				func(p int) { s.bmgrProgress(j, "Uploading to remote storage", p) })
			s.bmgrUpdate(j, func(j *bmgrJob) { j.Remote = key })
			s.bmgrFinish(j, err)
		}()
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups&job="+j.ID))
	case "remote-fetch":
		name := c.PostForm("fetch")
		if !bmgrRemoteNameRe.MatchString(name) {
			c.Redirect(http.StatusFound, s.bmgrRemoteBack("&bmgr-error="+url.QueryEscape("Not an encrypted backup name.")))
			return
		}
		pass := c.PostForm("passphrase")
		j, err := s.bmgrStart("fetch")
		if err != nil {
			c.Redirect(http.StatusFound, s.bmgrRemoteBack(bmgrErrParam(err)))
			return
		}
		go func() {
			jctx := s.bmgrCancelable(j)
			s.bmgrProgress(j, "Downloading from remote storage", 0)
			local, err := s.bmgrRemoteFetch(jctx, name, pass, func(f string, a ...any) { s.bmgrLog(j, f, a...) },
				func(p int) { s.bmgrProgress(j, "Downloading from remote storage", p) })
			s.bmgrUpdate(j, func(j *bmgrJob) { j.File = local })
			s.bmgrFinish(j, err)
		}()
		c.Redirect(http.StatusFound, s.bmgrRemoteBack("&job="+j.ID))
	default:
		c.Redirect(http.StatusFound, s.bmgrRemoteBack(""))
	}
}
