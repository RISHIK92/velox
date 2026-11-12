# Velox

Velox is a high-performance, containerized code execution engine (Online Judge) built with Go and Docker. It allows you to submit code in various languages, execute it against multiple test cases, and receive detailed resource usage (time and memory) along with execution status.

## Report Link
https://drive.google.com/file/d/1W_tCFVxVPxistrRpI8Y9a9LbJ5uf3Lv5/view?usp=sharing

## Project Structure

The project is split into two main services: an **API Server** and a **Worker**.

### backend/

Core Go application logic.

- **cmd/**
  - **api/**: Entry point for the HTTP server. Handles `/submit` and `/status` endpoints, protected by API Key authentication.
  - **worker/**: Entry point for the background worker. Continuously polls Redis for new submissions to process.
- **auth/**: Authentication module handling API Keys, Users, and background API usage metrics.
- **judge/**: Defines the data models (`judge.go`) for requests and responses used across the system.
- **processSubmission/**: The language orchestrator utilizing the Strategy Pattern. Handles compilation and script preparation for all supported languages.
- **runBatch/**: The execution engine. Runs binaries/scripts in a controlled environment, pipes input, and captures results, Time (ms), and Memory (KB).
- **shared/redis/**: Basic Redis wrapper for pushing/popping submissions and results.
- **docs/**: Automatically generated Swagger (OpenAPI 3.0) documentation.

---

## Judge jobs and the sandboxed engine

Velox also judges Interval's jobs, in Interval's `JudgeJob`/`JudgeResult` contract:

- `POST /judge` (API-key scope `submit`) queues a job at its priority (ranked, rooms, casual, run) and returns `202 {"id": …}`. A job outside the contract gets 400.
- `GET /judge/{id}` (scope `status`) returns the `JudgeResult`, or 404 while the job is pending.

The `judge-worker` service (`backend/cmd/exec serve`) takes those jobs. Each run gets a fresh, locked-down container on the host's Docker:

- no network, a read-only root, a non-root user and no capabilities;
- pids, memory and CPU limits;
- gVisor with `EXEC_RUNTIME=runsc`.

The checker judges a run's output outside the sandbox.

- **Packages:**
  - `backend/engine`: the sandbox and the language strategies (Python, JavaScript, TypeScript, Java, C++, Go).
  - `backend/checker`: the checker.
  - `backend/judgejob`: the queue, the workers and the HTTP handlers.
  - `backend/lld`: the LLD scenario types.
  - `backend/redteam`: the red-team suite ([docs/runbooks/redteam.md](docs/runbooks/redteam.md)).
- **Generated:** `backend/lld/types.go`, `backend/judgejob/types.go` and `backend/contract/schema/` are generated in Interval (`pnpm contracts:velox`). `backend/contract` tests that they agree.
- **The `exec` binary** (`go build ./cmd/exec`) also has `run` and `check` for local tooling.

## API Documentation

The project uses Swagger (OpenAPI 3.0) for API documentation.

### View Documentation
When the server is running in development mode (`GO_ENV=development`), you can access the interactive Swagger UI at:
- **[http://localhost:8080/swagger/index.html](http://localhost:8080/swagger/index.html)**

### Generate Documentation
If you add new endpoints or update annotations, regenerate the documentation using:
```bash
cd backend
go run github.com/swaggo/swag/cmd/swag init -g cmd/api/main.go
```

### build/

Contains infrastructure configuration.

- **Dockerfile.api**: Minimal Go runtime for the API service.
- **Dockerfile.worker**: Full-featured image containing compilers (gcc, g++, javac) and runtimes (python, node, openjdk) required for judging.

### docker-compose.yml

Orchestrates the `api`, `worker`, `postgres`, and `redis` services into a single local environment.

---

## Technology Stack

- **Language**: Go (Golang)
- **Database**: PostgreSQL
- **Queue/Store**: Redis
- **Containerization**: Docker & Docker Compose
- **Supported Languages**:
  - C (GCC 12+)
  - C++ (G++ 12+)
  - Java (OpenJDK 17)
  - C#
  - Python (3.x)
  - Node.js
  - TypeScript

---

## Quick Start

Ensure you have **Docker** and **Docker Compose** installed.

1. **Clone the repository:**

   ```bash
   git clone https://github.com/RISHIK92/velox.git
   cd velox
   ```

2. **Spin up the stack:**

   ```bash
   docker compose up --build
   ```

3. **Test a submission:**

   ```bash
   curl -X POST http://localhost:8080/submit \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer <YOUR_API_KEY>" \
     -d '{
       "language": "cpp",
       "source_code": "#include <iostream>\nusing namespace std;\nint main() { int a, b; while (cin >> a >> b) cout << a + b << endl; return 0; }",
       "test_cases": [{"test_case_id": 1, "input": "5 10", "expected_output": "15"}]
     }'
   ```

4. **Check status:**
   ```bash
   curl -X GET "http://localhost:8080/status?submission_id=<ID_FROM_PREVIOUS_STEP>" \
     -H "Authorization: Bearer <YOUR_API_KEY>"
   ```

---

## Contributing

We welcome contributions! To help you get started:

### Adding a New Language Support

1. **Create Strategy**: Under `backend/processSubmission/`, create a new file (e.g., `ruby_strategy.go`) implementing the `LanguageStrategy` interface.
2. **Register Strategy**: Update `NewDefaultRegistry()` in the `processSubmission` module to register your new strategy with its corresponding language key.
3. **Update Dockerfile**: Update `build/Dockerfile.worker` to ensure the necessary compiler or runtime is installed in the worker image.
4. **Add Tests**: Add unit tests for your strategy execution path.

### Development Workflow

- **Code Style**: We follow standard Go formatting (`go fmt`).
- **Testing**: Use the provided `curl` samples and test scripts to verify your changes.
- **Watch Mode**: You can use `docker compose watch` (if supported by your version) to automatically rebuild services when files in `backend/` change.

### Steps to Contribute

1. Fork the repo.
2. Create your feature branch (`git checkout -b feature/AmazingFeature`).
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`).
4. Push to the branch (`git push origin feature/AmazingFeature`).
5. Open a Pull Request.
