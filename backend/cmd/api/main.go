package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rishik92/velox/auth/db"
	"github.com/rishik92/velox/auth/handler"
	"github.com/rishik92/velox/auth/middleware"
	"github.com/rishik92/velox/auth/model"
	"github.com/rishik92/velox/auth/repository"
	"github.com/rishik92/velox/auth/service"
	"github.com/rishik92/velox/judge"
	judgejob "github.com/rishik92/velox/judgejob"
	veloxRedis "github.com/rishik92/velox/shared/redis"

	_ "github.com/rishik92/velox/docs"
	httpSwagger "github.com/swaggo/http-swagger"
)

// @title Velox API
// @version 1.0
// @description This is the Velox API server.
// @termsOfService http://swagger.io/terms/

// @contact.name API Support
// @contact.url http://www.swagger.io/support
// @contact.email support@swagger.io

// @license.name Apache 2.0
// @license.url http://www.apache.org/licenses/LICENSE-2.0.html

// @host localhost:8080
// @BasePath /

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Type "Bearer" followed by a space and then your personal token.

func main() {
	veloxRedis.Connect()

	// Connect to DB
	database, err := db.Connect()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close()

	if err := database.Ping(); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}
	fmt.Println("Connected to PostgreSQL database successfully.")

	// Run database migrations
	if err := db.RunMigrations(database); err != nil {
		log.Fatalf("failed to run database migrations: %v", err)
	}

	// Set up Auth Module
	repo := repository.NewUserRepository(database)
	svc := service.NewAuthService(repo)
	authHandler := handler.NewAuthHandler(svc)

	// Set up Dashboard Module
	dashboardSvc := service.NewDashboardService(repo)
	dashboardHandler := handler.NewDashboardHandler(dashboardSvc)

	// Set up API Key Module
	apiKeyRepo := repository.NewAPIKeyRepository(database)
	apiKeySvc := service.NewAPIKeyService(apiKeyRepo)
	
	// Set up API Logging Module
	logRepo := repository.NewAPILogRepository(database)
	apiLogSvc := service.NewAPILogService(logRepo)

	apiKeyHandler := handler.NewAPIKeyHandler(apiKeySvc, apiLogSvc)
	apiKeyMiddleware := middleware.NewAPIKeyAuthMiddleware(apiKeySvc)

	// Set up Judge Module
	judgeHandler := &JudgeHandler{
		apiLogSvc: apiLogSvc,
	}

	// Auth Routes
	http.HandleFunc("/auth/signup", authHandler.Signup)
	http.HandleFunc("/auth/login", authHandler.Login)
	http.HandleFunc("/auth/logout", authHandler.Logout)

	// API Key Management (Require Session Auth)
	http.Handle("POST /auth/api-keys", middleware.RequireAuth(http.HandlerFunc(apiKeyHandler.GenerateKey)))
	http.Handle("GET /auth/api-keys", middleware.RequireAuth(http.HandlerFunc(apiKeyHandler.ListKeys)))
	http.Handle("PATCH /auth/api-keys", middleware.RequireAuth(http.HandlerFunc(apiKeyHandler.UpdateKey)))
	http.Handle("DELETE /auth/api-keys", middleware.RequireAuth(http.HandlerFunc(apiKeyHandler.DeleteKey)))
	http.Handle("GET /auth/api-keys/stats", middleware.RequireAuth(http.HandlerFunc(apiKeyHandler.GetStats)))

	// Protected Dashboard Route
	dashboardMux := http.NewServeMux()
	dashboardMux.HandleFunc("/dashboard", dashboardHandler.GetData)
	http.Handle("/dashboard", middleware.RequireAuth(dashboardMux))

	// Submission API (Protected by API Key)
	submitMux := http.NewServeMux()
	submitMux.HandleFunc("/submit", judgeHandler.Submit)
	http.Handle("/submit", apiKeyMiddleware.Authenticate(middleware.CheckScope("submit", submitMux)))

	statusMux := http.NewServeMux()
	statusMux.HandleFunc("/status", judgeHandler.Status)
	http.Handle("/status", apiKeyMiddleware.Authenticate(middleware.CheckScope("status", statusMux)))

	// Judge jobs, in Interval's JudgeJob/JudgeResult contract (README, "Judge jobs"). Only
	// keys an operator lists may use them: any signed-up user can mint a key and pick its
	// scopes, and a judge job's priority (ranked first) is the caller's to choose.
	judgeStore := judgejob.Store{Client: veloxRedis.Client, Prefix: "judge"}
	judgeKeys := judgeKeyGate(os.Getenv("JUDGE_API_KEY_IDS"))
	http.Handle("POST /judge", apiKeyMiddleware.Authenticate(logged(apiLogSvc, "/judge", judgeKeys(judgeStore.SubmitHandler(apiKeyID)))))
	http.Handle("GET /judge/{id}", apiKeyMiddleware.Authenticate(logged(apiLogSvc, "/judge/{id}", judgeKeys(judgeStore.ResultHandler(apiKeyID)))))

	// General API
	http.HandleFunc("/health", healthHandler)

	// Swagger documentation (only in development)
	env := os.Getenv("GO_ENV")
	if env == "" || env == "development" {
		http.HandleFunc("/swagger/", httpSwagger.WrapHandler)
		fmt.Println("Swagger UI available at: http://localhost:8080/swagger/index.html")
	}

	fmt.Println("API Server running on :8080")
	handler := middleware.SecurityHeaders(corsMiddleware(http.DefaultServeMux))
	log.Fatal(http.ListenAndServe(":8080", handler))
}

type JudgeHandler struct {
	apiLogSvc *service.APILogService
}

// Submit is an HTTP handler to submit code for execution.
// @Summary Submit Code
// @Description Submit source code for execution against test cases.
// @Tags Judge
// @Accept json
// @Produce json
// @Security Bearer
// @Param request body judge.SubmissionRequest true "Submission Request"
// @Success 202 {object} map[string]string "submission_id"
// @Failure 400 {string} string "Invalid JSON or limits too high"
// @Failure 401 {string} string "Unauthorized"
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {string} string "Failed to queue submission"
// @Router /submit [post]
func (h *JudgeHandler) Submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiKeyID, _ := r.Context().Value(middleware.APIKeyIDKey).(string)

	var req judge.SubmissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.TimeLimitMs > 5000 || req.MemoryLimitKb > 512000 {
		http.Error(w, "Invalid request: Limits too high (Max 5s, 512MB)", http.StatusBadRequest)
		return
	}

	req.SubmissionID = uuid.New().String()

	startTime := time.Now()
	
	// Push to Redis
	raw, _ := json.Marshal(req)
	if err := veloxRedis.PushResult("submissions", string(raw)); err != nil {
		http.Error(w, "Failed to queue submission", http.StatusInternalServerError)
		return
	}

	// Async Log the initial request
	h.apiLogSvc.Log(&model.APILog{
		APIKeyID:     apiKeyID,
		SubmissionID: req.SubmissionID,
		Endpoint:     "/submit",
		Method:       "POST",
		StatusCode:   http.StatusAccepted,
		DurationMs:   int(time.Since(startTime).Milliseconds()),
		Language:     req.Language,
		OverallState: "Pending",
		CreatedAt:    time.Now(),
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `{"submission_id": "%s"}`, req.SubmissionID)
}

// Status retrieves the execution result of a submission.
// @Summary Check Submission Status
// @Description Get the current status and results of a code submission.
// @Tags Judge
// @Produce json
// @Security Bearer
// @Param submission_id query string true "Submission ID"
// @Success 200 {object} judge.SubmissionResponse "Submission Response (omitted if pending)"
// @Failure 400 {string} string "Missing submission_id"
// @Failure 401 {string} string "Unauthorized"
// @Router /status [get]
func (h *JudgeHandler) Status(w http.ResponseWriter, r *http.Request) {
	subID := r.URL.Query().Get("submission_id")
	if subID == "" {
		http.Error(w, "Missing submission_id", http.StatusBadRequest)
		return
	}

	apiKeyID, _ := r.Context().Value(middleware.APIKeyIDKey).(string)

	startTime := time.Now()

	// Check Redis for result
	resultQueue := "results:" + subID
	raw, found := veloxRedis.PopSubmission(resultQueue, 1*time.Second)

	if !found {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status": "pending"}`)
		return
	}

	// Parse result to log detailed state
	var res judge.SubmissionResponse
	if err := json.Unmarshal([]byte(raw), &res); err == nil {
		// Update the initial log with the final result
		h.apiLogSvc.UpdateResult(subID, res.OverallState, res.CompileError)

		// Also log THIS status request itself
		h.apiLogSvc.Log(&model.APILog{
			APIKeyID:     apiKeyID,
			Endpoint:     "/status",
			Method:       "GET",
			StatusCode:   http.StatusOK,
			DurationMs:   int(time.Since(startTime).Milliseconds()),
			OverallState: res.OverallState,
			CreatedAt:    time.Now(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(raw))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := os.Getenv("GO_ENV")

		var allowedOrigin string
		if env == "" || env == "development" {
			allowedOrigin = "*"
		} else {
			allowedOrigin = "https://example.com"
		}

		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// @Summary Health Check
// @Description Check if the API server is healthy.
// @Tags General
// @Produce json
// @Success 200 {object} map[string]string "Healthy"
// @Router /health [get]

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
	})
}

func apiKeyID(r *http.Request) string {
	id, _ := r.Context().Value(middleware.APIKeyIDKey).(string)
	return id
}

// judgeKeyGate admits only the API key IDs in a comma-separated list (JUDGE_API_KEY_IDS);
// with none listed, /judge is closed.
func judgeKeyGate(list string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowed[apiKeyID(r)] {
				http.Error(w, "this key may not submit judge jobs", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logged records each call in the API log, as /submit and /status are, so judge jobs
// show in a key's stats and audit trail; refused keys (403) included.
func logged(logs *service.APILogService, endpoint string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// api_logs.submission_id is a UUID: a malformed id from the path isn't one.
		var submission string
		if id, err := uuid.Parse(r.PathValue("id")); err == nil {
			submission = id.String()
		}
		logs.Log(&model.APILog{
			APIKeyID:     apiKeyID(r),
			SubmissionID: submission,
			Endpoint:     endpoint,
			Method:       r.Method,
			StatusCode:   rec.status,
			DurationMs:   int(time.Since(start).Milliseconds()),
			CreatedAt:    time.Now(),
		})
	})
}
