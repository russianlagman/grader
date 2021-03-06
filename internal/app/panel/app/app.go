package app

import (
	"database/sql"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-redis/redis/v8"
	_ "github.com/lib/pq"
	"grader/internal/app/panel/config"
	"grader/internal/app/panel/handler"
	"grader/internal/app/panel/pkg/auth"
	"grader/internal/app/panel/storage/postgres"
	"grader/internal/pkg/migrate"
	"grader/pkg/aws"
	"grader/pkg/httpserver"
	"grader/pkg/layout"
	"grader/pkg/logger"
	mw "grader/pkg/middleware"
	"grader/pkg/queue"
	"grader/pkg/queue/amqp"
	"grader/pkg/session"
	"grader/pkg/token"
	"grader/pkg/workerpool"
	"grader/web"
	"net/http"
	"runtime"
	"time"
)

type App struct {
	config  config.Config
	logger  logger.Logger
	stop    chan struct{}
	queue   queue.Queue
	workers *workerpool.Pool
	server  *httpserver.Server
	s3      *aws.S3
}

func New(cfg config.Config) (*App, error) {
	l := *logger.Global()

	wp := workerpool.New()

	db, err := sql.Open("postgres", cfg.DB.DSN)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("db ping: %w", err)
	}
	if err := migrate.Up(db); err != nil {
		return nil, fmt.Errorf("migrate up: %w", err)
	}

	// init amqp dep
	q, err := amqp.New(cfg.AMQP)
	if err != nil {
		return nil, fmt.Errorf("amqp: %w", err)
	}

	rds := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Host,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	tm, err := token.NewJWT(cfg.Security.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("token manager: %w", err)
	}

	sm := session.NewRedis(
		rds,
		tm,
		session.WithSessionLifetime(1*time.Hour),
	)

	s3, err := aws.NewS3(cfg.AWS)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}

	users, err := postgres.NewUserRepository(db)
	if err != nil {
		return nil, fmt.Errorf("user repository: %w", err)
	}
	assessments, err := postgres.NewAssessmentRepository(db)
	if err != nil {
		return nil, fmt.Errorf("assessments repository: %w", err)
	}
	submissions, err := postgres.NewSubmissionRepository(db)
	if err != nil {
		return nil, fmt.Errorf("assessments repository: %w", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(mw.Log(l))

	lt, err := layout.NewLayout(
		web.TemplatesFS,
		"template/app/layouts/base.gohtml",
		handler.ViewDataFunc(tm),
	)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}

	uh := handler.NewUserHandler(lt, sm, users)
	ah := handler.NewAdminHandler(lt, users, assessments)
	sh, err := handler.NewSubmitHandler(lt, s3, q, cfg.App.TopicName, users, assessments, submissions)
	if err != nil {
		return nil, fmt.Errorf("submission handler: %w", err)
	}

	r.Route("/app", func(r chi.Router) {
		r.Use(session.ContextMiddleware(sm))
		r.Use(auth.ContextMiddleware(users))

		r.Route("/submit", func(r chi.Router) {
			r.Use(auth.AuthMiddleware())

			r.Get("/{id}", sh.Create)
			r.Post("/{id}", sh.Create)
		})

		r.Route("/user", func(r chi.Router) {
			r.Get("/login", uh.Login)
			r.Post("/login", uh.Login)

			r.Get("/register", uh.Register)
			r.Post("/register", uh.Register)

			r.Get("/logout", uh.Logout)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(auth.AuthMiddleware())

			r.Get("/assessments", ah.AssessmentList)

			r.Get("/assessments/create", ah.AssessmentCreate)
			r.Post("/assessments/create", ah.AssessmentCreate)
		})

		r.Get("/", uh.Default)
	})

	static := http.FileServer(http.FS(web.StaticFS))
	r.Handle("/static/*", static)

	hs, err := httpserver.New(cfg.Server, r)
	if err != nil {
		return nil, fmt.Errorf("http server: %w", err)
	}

	a := &App{
		config:  cfg,
		logger:  l,
		stop:    make(chan struct{}),
		queue:   q,
		server:  hs,
		workers: wp,
		s3:      s3,
	}

	go func() {
		<-a.stop
		q.Stop()
	}()

	wp.Start(runtime.GOMAXPROCS(0) * 2)

	return a, nil
}

func (a *App) Stop() {
	close(a.stop)
	a.server.Stop()
	a.workers.Stop()
}
