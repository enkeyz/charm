package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"

	charmfs "github.com/charmbracelet/charm/fs"
	charm "github.com/charmbracelet/charm/proto"
	"github.com/charmbracelet/charm/server/db"
	"github.com/charmbracelet/charm/server/storage"
	"github.com/meowgorithm/babylogger"
	"goji.io"
	"goji.io/pat"
	"goji.io/pattern"
	"gopkg.in/square/go-jose.v2"
)

const resultsPerPage = 50

// HTTPServer is the HTTP server for the Charm Cloud backend.
type HTTPServer struct {
	db      db.DB
	fstore  storage.FileStore
	cfg     *Config
	handler http.Handler
	server  *http.Server
	health  *http.Server
}

type providerJSON struct {
	Issuer      string   `json:"issuer"`
	AuthURL     string   `json:"authorization_endpoint"`
	TokenURL    string   `json:"token_endpoint"`
	JWKSURL     string   `json:"jwks_uri"`
	UserInfoURL string   `json:"userinfo_endpoint"`
	Algorithms  []string `json:"id_token_signing_alg_values_supported"`
}

// NewHTTPServer returns a new *HTTPServer with the specified Config.
func NewHTTPServer(cfg *Config) (*HTTPServer, error) {
	healthMux := http.NewServeMux()
	// No auth health check endpoint
	healthMux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "We live!")
	}))
	health := &http.Server{
		Addr:      fmt.Sprintf("%s:%d", cfg.Host, cfg.HealthPort),
		Handler:   healthMux,
		TLSConfig: cfg.tlsConfig,
		ErrorLog:  cfg.errorLog,
	}
	mux := goji.NewMux()
	s := &HTTPServer{
		cfg:     cfg,
		handler: mux,
		health:  health,
	}
	jwtMiddleware, err := JWTMiddleware(
		cfg.jwtKeyPair.JWK.Public(),
		cfg.httpURL(),
		[]string{"charm"},
	)
	if err != nil {
		return nil, err
	}

	mux.Use(babylogger.Middleware)
	mux.Use(PublicPrefixesMiddleware([]string{"/v1/public/", "/.well-known/"}))
	mux.Use(jwtMiddleware)
	mux.Use(CharmUserMiddleware(s))
	mux.Use(RequestLimitMiddleware())
	mux.HandleFunc(pat.Get("/v1/id/:id"), s.handleGetUserByID)
	mux.HandleFunc(pat.Get("/v1/bio/:name"), s.handleGetUser)
	mux.HandleFunc(pat.Post("/v1/bio"), s.handlePostUser)
	mux.HandleFunc(pat.Post("/v1/encrypt-key"), s.handlePostEncryptKey)
	mux.HandleFunc(pat.Get("/v1/fs/*"), s.handleGetFile)
	mux.HandleFunc(pat.Post("/v1/fs/*"), s.handlePostFile)
	mux.HandleFunc(pat.Delete("/v1/fs/*"), s.handleDeleteFile)
	mux.HandleFunc(pat.Get("/v1/seq/:name"), s.handleGetSeq)
	mux.HandleFunc(pat.Post("/v1/seq/:name"), s.handlePostSeq)
	mux.HandleFunc(pat.Get("/v1/news"), s.handleGetNewsList)
	mux.HandleFunc(pat.Get("/v1/news/:id"), s.handleGetNews)
	mux.HandleFunc(pat.Get("/v1/public/jwks"), s.handleJWKS)
	mux.HandleFunc(pat.Get("/.well-known/openid-configuration"), s.handleOpenIDConfig)
	s.db = cfg.DB
	s.fstore = cfg.FileStore
	return s, nil
}

// Start start the HTTP and health servers on the ports specified in the Config.
func (s *HTTPServer) Start() {
	useTLS := s.cfg.httpScheme == "https"
	listenAddr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.HTTPPort)
	s.server = &http.Server{
		Addr:      listenAddr,
		Handler:   s.handler,
		TLSConfig: s.cfg.tlsConfig,
		ErrorLog:  s.cfg.errorLog,
	}

	go func() {
		log.Printf("Starting %s health server on: %s", strings.ToUpper(s.cfg.httpScheme), s.health.Addr)
		f := "http health endpoint server exited with error: %s"
		if useTLS {
			log.Fatalf(f, s.health.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile))
		} else {
			log.Fatal(f, s.health.ListenAndServe())
		}
	}()

	log.Printf("Starting %s server on: %s", strings.ToUpper(s.cfg.httpScheme), listenAddr)
	if useTLS {
		log.Fatalf("Server crashed: %s", s.server.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile))
	} else {
		log.Fatalf("Server crashed: %s", s.server.ListenAndServe())
	}
}

// Shutdown gracefully shut down the HTTP and health servers.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	log.Printf("Stopping %s server on %s", strings.ToUpper(s.cfg.httpScheme), s.server.Addr)
	if err := s.health.Shutdown(ctx); err != nil {
		return err
	}
	return s.server.Shutdown(ctx)
}

func (s *HTTPServer) renderError(w http.ResponseWriter) {
	s.renderCustomError(w, "internal error", http.StatusInternalServerError)
}

func (s *HTTPServer) renderCustomError(w http.ResponseWriter, msg string, status int) {
	w.WriteHeader(status)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(charm.Message{Message: msg})
}

func (s *HTTPServer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{s.cfg.jwtKeyPair.JWK.Public()}}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(jwks)
}

func (s *HTTPServer) handleOpenIDConfig(w http.ResponseWriter, r *http.Request) {
	pj := providerJSON{JWKSURL: fmt.Sprintf("%s/v1/public/jwks", s.cfg.httpURL())}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(pj)
}

// TODO do we need this since you can only get the authed user?
func (s *HTTPServer) handleGetUserByID(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(u)
	s.cfg.Stats.GetUserByID()
}

// TODO do we need this since you can only get the authed user?
func (s *HTTPServer) handleGetUser(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(u)
	s.cfg.Stats.GetUser()
}

func (s *HTTPServer) handlePostUser(w http.ResponseWriter, r *http.Request) {
	id, err := charmIDFromRequest(r)
	if err != nil {
		log.Printf("cannot read request body: %s", err)
		s.renderError(w)
		return
	}
	u := &charm.User{}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("cannot read request body: %s", err)
		s.renderError(w)
		return
	}
	err = json.Unmarshal(body, u)
	if err != nil {
		log.Printf("cannot decode user json: %s", err)
		s.renderError(w)
		return
	}
	nu, err := s.db.SetUserName(id, u.Name)
	if err == charm.ErrNameTaken {
		s.renderCustomError(w, fmt.Sprintf("username '%s' already taken", u.Name), http.StatusConflict)
	} else if err != nil {
		log.Printf("cannot set user name: %s", err)
		s.renderError(w)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nu)
	s.cfg.Stats.SetUserName()
}

func (s *HTTPServer) handlePostEncryptKey(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	ek := &charm.EncryptKey{}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("cannot read request body: %s", err)
		s.renderError(w)
		return
	}
	err = json.Unmarshal(body, ek)
	if err != nil {
		log.Printf("cannot decode encrypt key json: %s", err)
		s.renderError(w)
		return
	}
	err = s.db.AddEncryptKeyForPublicKey(u, ek.PublicKey, ek.ID, ek.Key, ek.CreatedAt)
	if err != nil {
		log.Printf("cannot add encrypt key: %s", err)
		s.renderError(w)
		return
	}
	s.cfg.Stats.SetUserName()
}

func (s *HTTPServer) handleGetSeq(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	name := pat.Param(r, "name")
	seq, err := s.db.GetSeq(u, name)
	if err != nil {
		log.Printf("cannot get seq: %s", err)
		s.renderError(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&charm.SeqMsg{Seq: seq})
}

func (s *HTTPServer) handlePostSeq(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	name := pat.Param(r, "name")
	seq, err := s.db.NextSeq(u, name)
	if err != nil {
		log.Printf("cannot get next seq: %s", err)
		s.renderError(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&charm.SeqMsg{Seq: seq})
}

func (s *HTTPServer) handlePostFile(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	path := pattern.Path(r.Context())
	ms := r.URL.Query().Get("mode")
	m, err := strconv.ParseUint(ms, 10, 32)
	if err != nil {
		log.Printf("file mode not a number: %s", err)
		s.renderError(w)
		return
	}
	f, _, err := r.FormFile("data")
	if err != nil {
		log.Printf("cannot parse form data: %s", err)
		s.renderError(w)
		return
	}
	defer f.Close()
	err = s.cfg.FileStore.Put(u.CharmID, path, f, fs.FileMode(m))
	if err != nil {
		log.Printf("cannot post file: %s", err)
		s.renderError(w)
		return
	}
}

func (s *HTTPServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	path := pattern.Path(r.Context())
	f, err := s.cfg.FileStore.Get(u.CharmID, path)
	if errors.Is(err, fs.ErrNotExist) {
		s.renderCustomError(w, "file not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("cannot get file: %s", err)
		s.renderError(w)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		log.Printf("cannot get file info: %s", err)
		s.renderError(w)
		return
	}

	switch f.(type) {
	case *charmfs.DirFile:
		w.Header().Set("Content-Type", "application/json")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Last-Modified", fi.ModTime().Format(http.TimeFormat))
	}
	w.Header().Set("X-File-Mode", fmt.Sprintf("%d", fi.Mode()))
	_, err = io.Copy(w, f)
	if err != nil {
		log.Printf("cannot copy file: %s", err)
		s.renderError(w)
		return
	}
}

func (s *HTTPServer) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	u := s.charmUserFromRequest(w, r)
	path := pattern.Path(r.Context())
	err := s.cfg.FileStore.Delete(u.CharmID, path)
	if err != nil {
		log.Printf("cannot delete file: %s", err)
		s.renderError(w)
		return
	}
}

func (s *HTTPServer) handleGetNewsList(w http.ResponseWriter, r *http.Request) {
	p := r.FormValue("page")
	if p == "" {
		p = "1"
	}
	page, err := strconv.Atoi(p)
	if err != nil {
		log.Printf("page not a number: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	offset := (page - 1) * resultsPerPage
	tag := r.FormValue("tag")
	if tag == "" {
		tag = "server"
	}
	ns, err := s.db.GetNewsList(tag, offset)
	if err != nil {
		log.Printf("cannot get news: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ns)
	s.cfg.Stats.GetNews()
}

func (s *HTTPServer) handleGetNews(w http.ResponseWriter, r *http.Request) {
	id := pat.Param(r, "id")
	news, err := s.db.GetNews(id)
	if err != nil {
		log.Printf("cannot get news markdown: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(news)
	s.cfg.Stats.GetNews()
}

func (s *HTTPServer) charmUserFromRequest(w http.ResponseWriter, r *http.Request) *charm.User {
	u := r.Context().Value(ctxUserKey)
	if u == nil {
		log.Printf("could not assign user to request context")
		s.renderError(w)
	}
	return u.(*charm.User)
}
