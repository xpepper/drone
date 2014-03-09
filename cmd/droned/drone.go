package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"code.google.com/p/go.net/websocket"
	"github.com/GeertJohan/go.rice"
	"github.com/bmizerany/pat"
	_ "github.com/mattn/go-sqlite3"
	"github.com/russross/meddler"

	"github.com/drone/drone/pkg/build/docker"
	"github.com/drone/drone/pkg/channel"
	"github.com/drone/drone/pkg/database"
	"github.com/drone/drone/pkg/database/migrate"
	"github.com/drone/drone/pkg/handler"
	"github.com/drone/drone/pkg/queue"
)

var (
	// local path where the SQLite database
	// should be stored. By default this is
	// in the current working directory.
	path string

	// port the server will run on
	port string

	// database driver used to connect to the database
	driver string

	// driver specific connection information. In this
	// case, it should be the location of the SQLite file
	datasource string

	// optional flags for tls listener
	sslcert string
	sslkey  string

	// build will timeout after N milliseconds.
	// this will default to 500 minutes (6 hours)
	timeout time.Duration

	// commit sha for the current build.
	version string
)

func main() {
	// parse command line flags
	flag.StringVar(&path, "path", "", "")
	flag.StringVar(&port, "port", ":8080", "")
	flag.StringVar(&driver, "driver", "sqlite3", "")
	flag.StringVar(&datasource, "datasource", "drone.sqlite", "")
	flag.StringVar(&sslcert, "sslcert", "", "")
	flag.StringVar(&sslkey, "sslkey", "", "")
	flag.DurationVar(&timeout, "timeout", 300*time.Minute, "")
	flag.Parse()

	// validate the TLS arguments
	checkTLSFlags()

	// setup database and handlers
	setupDatabase()
	setupStatic()
	setupHandlers()

	// debug
	log.Printf("starting drone version %s on port %s\n", version, port)

	// start webserver using HTTPS or HTTP
	if sslcert != "" && sslkey != "" {
		panic(http.ListenAndServeTLS(port, sslcert, sslkey, nil))
	} else {
		panic(http.ListenAndServe(port, nil))
	}
}

// checking if the TLS flags where supplied correctly.
func checkTLSFlags() {

	if sslcert != "" && sslkey == "" {
		log.Fatal("invalid configuration: -sslkey unspecified, but -sslcert was specified.")
	} else if sslcert == "" && sslkey != "" {
		log.Fatal("invalid configuration: -sslcert unspecified, but -sslkey was specified.")
	}

}

// setup the database connection and register with the
// global database package.
func setupDatabase() {
	// inform meddler and migration we're using sqlite
	meddler.Default = meddler.SQLite
	migrate.Driver = migrate.SQLite

	// connect to the SQLite database
	db, err := sql.Open(driver, datasource)
	if err != nil {
		log.Fatal(err)
	}

	database.Set(db)

	migration := migrate.New(db)
	migration.All().Migrate()
}

// setup routes for static assets. These assets may
// be directly embedded inside the application using
// the `rice embed` command, else they are served from disk.
func setupStatic() {
	box := rice.MustFindBox("assets")
	http.Handle("/css/", http.FileServer(box.HTTPBox()))
	http.Handle("/js/", http.FileServer(box.HTTPBox()))

	// we need to intercept all attempts to serve images
	// so that we can add a cache-control settings
	var images = http.FileServer(box.HTTPBox())
	http.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/img/build_") {
			w.Header().Add("Cache-Control", "no-cache")
		}

		// serce images
		images.ServeHTTP(w, r)
	})
}

// setup routes for serving dynamic content.
func setupHandlers() {
	queueRunner := queue.NewBuildRunner(docker.New(), timeout)
	queue := queue.Start(runtime.NumCPU(), queueRunner)

	hookHandler := handler.NewHookHandler(queue)

	m := pat.New()
	m.Get("/login", handler.ErrorHandler(handler.Login))
	m.Post("/login", handler.ErrorHandler(handler.Authorize))
	m.Get("/logout", handler.ErrorHandler(handler.Logout))
	m.Get("/forgot", handler.ErrorHandler(handler.Forgot))
	m.Post("/forgot", handler.ErrorHandler(handler.ForgotPost))
	m.Get("/reset", handler.ErrorHandler(handler.Reset))
	m.Post("/reset", handler.ErrorHandler(handler.ResetPost))
	m.Get("/signup", handler.ErrorHandler(handler.SignUp))
	m.Post("/signup", handler.ErrorHandler(handler.SignUpPost))
	m.Get("/register", handler.ErrorHandler(handler.Register))
	m.Post("/register", handler.ErrorHandler(handler.RegisterPost))
	m.Get("/accept", handler.UserHandler(handler.TeamMemberAccept))

	// handlers for setting up your GitHub repository
	m.Post("/new/github.com", handler.UserHandler(handler.RepoCreateGithub))
	m.Get("/new/github.com", handler.UserHandler(handler.RepoAdd))

	// handlers for linking your GitHub account
	m.Get("/auth/login/github", handler.UserHandler(handler.LinkGithub))

	// handlers for dashboard pages
	m.Get("/dashboard/team/:team", handler.UserHandler(handler.TeamShow))
	m.Get("/dashboard", handler.UserHandler(handler.UserShow))

	// handlers for user account management
	m.Get("/account/user/profile", handler.UserHandler(handler.UserEdit))
	m.Post("/account/user/profile", handler.UserHandler(handler.UserUpdate))
	m.Get("/account/user/delete", handler.UserHandler(handler.UserDeleteConfirm))
	m.Post("/account/user/delete", handler.UserHandler(handler.UserDelete))
	m.Get("/account/user/password", handler.UserHandler(handler.UserPass))
	m.Post("/account/user/password", handler.UserHandler(handler.UserPassUpdate))
	m.Get("/account/user/teams/add", handler.UserHandler(handler.TeamAdd))
	m.Post("/account/user/teams/add", handler.UserHandler(handler.TeamCreate))
	m.Get("/account/user/teams", handler.UserHandler(handler.UserTeams))

	// handlers for team managements
	m.Get("/account/team/:team/profile", handler.UserHandler(handler.TeamEdit))
	m.Post("/account/team/:team/profile", handler.UserHandler(handler.TeamUpdate))
	m.Get("/account/team/:team/delete", handler.UserHandler(handler.TeamDeleteConfirm))
	m.Post("/account/team/:team/delete", handler.UserHandler(handler.TeamDelete))
	m.Get("/account/team/:team/members/add", handler.UserHandler(handler.TeamMemberAdd))
	m.Post("/account/team/:team/members/add", handler.UserHandler(handler.TeamMemberInvite))
	m.Get("/account/team/:team/members/edit", handler.UserHandler(handler.TeamMemberEdit))
	m.Post("/account/team/:team/members/edit", handler.UserHandler(handler.TeamMemberUpdate))
	m.Post("/account/team/:team/members/delete", handler.UserHandler(handler.TeamMemberDelete))
	m.Get("/account/team/:team/members", handler.UserHandler(handler.TeamMembers))

	// handlers for system administration
	m.Get("/account/admin/settings", handler.AdminHandler(handler.AdminSettings))
	m.Post("/account/admin/settings", handler.AdminHandler(handler.AdminSettingsUpdate))
	m.Get("/account/admin/users/edit", handler.AdminHandler(handler.AdminUserEdit))
	m.Post("/account/admin/users/edit", handler.AdminHandler(handler.AdminUserUpdate))
	m.Post("/account/admin/users/delete", handler.AdminHandler(handler.AdminUserDelete))
	m.Get("/account/admin/users/add", handler.AdminHandler(handler.AdminUserAdd))
	m.Post("/account/admin/users", handler.AdminHandler(handler.AdminUserInvite))
	m.Get("/account/admin/users", handler.AdminHandler(handler.AdminUserList))

	// handlers for GitHub post-commit hooks
	m.Post("/hook/github.com", handler.ErrorHandler(hookHandler.Hook))

	// handlers for first-time installation
	m.Get("/install", handler.ErrorHandler(handler.Install))
	m.Post("/install", handler.ErrorHandler(handler.InstallPost))

	// handlers for repository, commits and build details
	m.Get("/:host/:owner/:name/commit/:commit/build/:label/out.txt", handler.RepoHandler(handler.BuildOut))
	m.Get("/:host/:owner/:name/commit/:commit/build/:label", handler.RepoHandler(handler.CommitShow))
	m.Get("/:host/:owner/:name/commit/:commit", handler.RepoHandler(handler.CommitShow))
	m.Get("/:host/:owner/:name/tree", handler.RepoHandler(handler.RepoDashboard))
	m.Get("/:host/:owner/:name/status.png", handler.ErrorHandler(handler.Badge))
	m.Get("/:host/:owner/:name/settings", handler.RepoAdminHandler(handler.RepoSettingsForm))
	m.Get("/:host/:owner/:name/params", handler.RepoAdminHandler(handler.RepoParamsForm))
	m.Get("/:host/:owner/:name/badges", handler.RepoAdminHandler(handler.RepoBadges))
	m.Get("/:host/:owner/:name/keys", handler.RepoAdminHandler(handler.RepoKeys))
	m.Get("/:host/:owner/:name/delete", handler.RepoAdminHandler(handler.RepoDeleteForm))
	m.Post("/:host/:owner/:name/delete", handler.RepoAdminHandler(handler.RepoDelete))
	m.Get("/:host/:owner/:name", handler.RepoHandler(handler.RepoDashboard))
	m.Post("/:host/:owner/:name", handler.RepoHandler(handler.RepoUpdate))
	http.Handle("/feed", websocket.Handler(channel.Read))

	// no routes are served at the root URL. Instead we will
	// redirect the user to his/her dashboard page.
	m.Get("/", http.RedirectHandler("/dashboard", http.StatusSeeOther))

	// the first time a page is requested we should record
	// the scheme and hostname.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// our multiplexer is a bit finnicky and therefore requires
		// us to strip any trailing slashes in order to correctly
		// find and match a route.
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path[:len(r.URL.Path)-1], http.StatusSeeOther)
			return
		}

		// standard header variables that should be set, for good measure.
		w.Header().Add("Cache-Control", "no-cache, no-store, max-age=0, must-revalidate")
		w.Header().Add("X-Frame-Options", "DENY")
		w.Header().Add("X-Content-Type-Options", "nosniff")
		w.Header().Add("X-XSS-Protection", "1; mode=block")

		// ok, now we're ready to serve the request.
		m.ServeHTTP(w, r)
	})
}
