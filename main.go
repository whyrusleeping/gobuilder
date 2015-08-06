package main

import (
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/s3"

	"github.com/Luzifer/gobuilder/builddb"
	"github.com/Luzifer/gobuilder/buildjob"
	"github.com/Luzifer/gobuilder/config"
	"github.com/flosch/pongo2"
	"github.com/gorilla/mux"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/xuyu/goredis"

	"github.com/Sirupsen/logrus"
	"github.com/Sirupsen/logrus/hooks/papertrail"

	_ "github.com/Luzifer/gobuilder/filters"
	_ "github.com/flosch/pongo2-addons"
)

var (
	s3Bucket     *s3.Bucket
	log          = logrus.New()
	redisClient  *goredis.Redis
	sessionStore *sessions.CookieStore
	cfg          *config.Config
)

type flashContext map[string]string

func init() {
	var err error
	log.Out = os.Stderr
	log.Formatter = &logrus.TextFormatter{ForceColors: true}

	cfg = config.Load()

	if cfg.Papertrail.Port != 0 {
		hook, err := logrus_papertrail.NewPapertrailHook(cfg.Papertrail.Host, cfg.Papertrail.Port, "GoBuilder Frontend")
		if err != nil {
			log.Panic("Unable to create papertrail connection")
			os.Exit(1)
		}

		log.Hooks.Add(hook)
	}

	redisClient, err = goredis.DialURL(cfg.RedisURL)
	if err != nil {
		log.WithFields(logrus.Fields{
			"url": cfg.RedisURL,
		}).Panic("Unable to connect to Redis")
		os.Exit(1)
	}

	sessionStoreAuthenticationKey := cfg.Session.AuthKey
	if sessionStoreAuthenticationKey == "" {
		sessionStoreAuthenticationKey = string(securecookie.GenerateRandomKey(32))
		log.Warn("The cookie authentication key was autogenerated. This will break sessions!")
	}
	sessionStoreEncryptionKey := cfg.Session.EncryptKey
	if sessionStoreEncryptionKey == "" {
		sessionStoreEncryptionKey = string(securecookie.GenerateRandomKey(32))
		log.Warn("The cookie encryption key was autogenerated. This will break sessions!")
	}

	sessionStore = sessions.NewCookieStore(
		[]byte(sessionStoreAuthenticationKey),
		[]byte(sessionStoreEncryptionKey),
	)

	gob.Register(&flashContext{})
}

func main() {
	connectS3()

	r := mux.NewRouter()
	registerAPIv1(r)

	r.PathPrefix("/css/").Handler(http.FileServer(http.Dir("./frontend/")))
	r.PathPrefix("/js/").Handler(http.FileServer(http.Dir("./frontend/")))
	r.PathPrefix("/fonts/").Handler(http.FileServer(http.Dir("./frontend/")))
	r.Handle("/favicon.ico", http.FileServer(http.Dir("./frontend/")))
	r.Handle("/robots.txt", http.FileServer(http.Dir("./frontend/")))

	// Static handlers
	r.HandleFunc("/", handleFrontPage).Methods("GET")
	r.HandleFunc("/contact", handleImprint).Methods("GET")
	r.HandleFunc("/help", handleHelpPage).Methods("GET")
	r.Handle("/metrics", prometheus.Handler())

	// GitHub auth
	r.HandleFunc("/ghlogin", handleOauthGithubInit).Methods("GET")
	r.HandleFunc("/ghlogout", handleOauthGithubLogout).Methods("GET")

	// Build starters / webhooks (deprecated bv /api/v1/webhook/*)
	r.HandleFunc("/webhook/github", webhookGitHub).Methods("POST")
	r.HandleFunc("/webhook/bitbucket", webhookBitBucket).Methods("POST")

	// Build artifact displaying
	r.HandleFunc("/get/{file:.+}", handlerDeliverFileFromS3).Methods("GET")
	r.HandleFunc("/{repo:.+}/log/{logid}", handlerBuildLog).Methods("GET")
	r.HandleFunc("/{repo:.+}", handlerRepositoryView).Methods("GET")

	http.Handle("/", httpAccessLog(r))

	if cfg.Port > 0 {
		cfg.Listen = fmt.Sprintf(":%d", cfg.Port)
	}

	http.ListenAndServe(cfg.Listen, nil)
}

func httpAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, r *http.Request) {
		wrappedResponseWriter := newAccessLogResponseWriter(res)
		start := time.Now()

		next.ServeHTTP(wrappedResponseWriter, r)

		duration := time.Now().Sub(start)
		log.Infof("=> %s \"%s\" %d %d %.2fms \"%s\" \"%s\"",
			strings.SplitN(r.RemoteAddr, ":", 2)[0],
			fmt.Sprintf("%s %s", r.Method, r.RequestURI),
			wrappedResponseWriter.StatusCode,
			wrappedResponseWriter.Size,
			float64(duration.Nanoseconds())/1000000.0,
			r.Referer(),
			r.UserAgent(),
		)
	})
}

func handlerRepositoryView(res http.ResponseWriter, r *http.Request) {
	sess, _ := sessionStore.Get(r, "GoBuilderSession")

	params := mux.Vars(r)
	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	buildStatus, err := redisClient.Get(fmt.Sprintf("project::%s::build-status", params["repo"]))
	if err != nil || buildStatus == nil {
		log.WithFields(logrus.Fields{
			"error": fmt.Sprintf("%v", err),
			"repo":  params["repo"],
		}).Warn("AWS S3 Get Error")

		sess.AddFlash(flashContext{
			"error": "Your build is not yet known to us...",
			"value": params["repo"],
		}, "context")
		sess.Save(r, res)
		http.Redirect(res, r, "/", http.StatusFound)
		return
	}

	readmeContent, err := s3Bucket.Get(fmt.Sprintf("%s/%s_README.md", params["repo"], branch))
	if err != nil {
		readmeContent = []byte("Project provided no README.md file.")
	}

	buildDurationRaw, err := redisClient.Get(fmt.Sprintf("project::%s::build-duration", params["repo"]))
	if err != nil || len(buildDurationRaw) == 0 {
		buildDurationRaw = []byte("0")
	}
	buildDuration, err := strconv.Atoi(string(buildDurationRaw))
	if err != nil {
		buildDuration = 0
	}

	signature, err := redisClient.Get(fmt.Sprintf("project::%s::signatures::%s", params["repo"], branch))
	if err != nil {
		signature = []byte("")
	}

	buildDB := builddb.BuildDB{}
	hasBuilds := false

	file, err := getBuildDBWithFallback(params["repo"])
	if err != nil {
		buildDB["master"] = builddb.Branch{}
		hasBuilds = false
	} else {
		err = json.Unmarshal(file, &buildDB)
		if err != nil {
			log.WithFields(logrus.Fields{
				"error": fmt.Sprintf("%v", err),
			}).Error("AWS DB Unmarshal Error")

			sess.AddFlash("Your build is not yet known to us...", "alert_error")
			sess.Save(r, res)
			http.Redirect(res, r, "/", http.StatusFound)
			return
		}
		hasBuilds = true
	}

	logs, err := redisClient.ZRevRange(fmt.Sprintf("project::%s::logs", params["repo"]), 0, 10, false)
	if err != nil {
		logs = []string{}
		log.WithFields(logrus.Fields{
			"repo": params["repo"],
			"err":  err,
		}).Error("Unable to load last logs")
	}
	logMetas := []*buildjob.BuildLog{}
	for _, v := range logs {
		if l, err := buildjob.LogFromString(v); err == nil {
			logMetas = append(logMetas, l)
		} else {
			// TODO: Remove me. I'm only here for migration purposes!
			logMetas = append(logMetas, &buildjob.BuildLog{
				ID: v,
			})
		}
	}

	abortReason, _ := redisClient.Get(fmt.Sprintf("project::%s::abort", params["repo"]))

	template := pongo2.Must(pongo2.FromFile("frontend/repository.html"))
	branches := []builddb.BranchSortEntry{}
	for k, v := range buildDB {
		branches = append(branches, builddb.BranchSortEntry{Branch: k, BuildDate: v.BuildDate})
	}
	sort.Sort(sort.Reverse(builddb.BranchSortEntryByBuildDate(branches)))

	ctx := getBasicContext(res, r)
	ctx["branch"] = branch
	ctx["branches"] = branches
	ctx["repo"] = params["repo"]
	ctx["mybranch"] = buildDB[branch]
	ctx["buildStatus"] = string(buildStatus)
	ctx["readme"] = string(readmeContent)
	ctx["hasbuilds"] = hasBuilds
	ctx["buildDuration"] = buildDuration
	ctx["signature"] = string(signature)
	ctx["logs"] = logMetas
	ctx["abort"] = string(abortReason)

	template.ExecuteWriter(ctx, res)
}

func connectS3() {
	s3auth, err := aws.EnvAuth()
	if err != nil {
		panic(err)
	}

	s3conn := s3.New(s3auth, aws.Regions["eu-west-1"])
	bucket := s3conn.Bucket("gobuild.luzifer.io")

	s3Bucket = bucket
}

func getBuildDBWithFallback(repo string) ([]byte, error) {
	redisKey := fmt.Sprintf("project::%s::builddb", repo)
	buildDB, err := redisClient.Get(redisKey)
	if err != nil || len(buildDB) == 0 {
		// Fall back to old storage method
		buildDB, err = s3Bucket.Get(fmt.Sprintf("%s/build.db", repo))
		if err != nil {
			return []byte{}, fmt.Errorf("Unable to load build.db: %s", err)
		}
	}

	return buildDB, nil
}
