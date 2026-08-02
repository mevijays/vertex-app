# Vertex Chat

Vertex Chat is a Go web application for Gemini 3.5 Flash on Google Cloud. It provides a responsive browser chat interface and an OpenAI-compatible Chat Completions endpoint, streams model output with Server-Sent Events (SSE), persists conversations and uploaded-document context in embedded SQLite, gates both the UI and the API behind a shared login token, and deploys as a keyless container on GKE.

## What is included

- `POST /v1/chat/completions` — an OpenAI-compatible Chat Completions endpoint, streamed or non-streamed. Works with the official `openai` SDK (Python/Node/etc.) by pointing `base_url` at this server — see [Using the OpenAI-compatible endpoint](#using-the-openai-compatible-endpoint).
- Gemini 3.5 Flash through the Google Gen AI SDK and Vertex AI.
- A single shared access token gates the whole app — the web UI and the API both authenticate with it. See [App authentication](#app-authentication-static-token).
- Document upload (PDF or plain text): Gemini extracts and summarizes the content, and that text is injected as context on later turns in the same conversation — from the web UI or from the API via `conversation_id`.
- Saved prompts and a "Manage data" screen (in the UI, after login) for browsing and deleting server-stored conversations, their documents, and saved prompts.
- Conversation history: the browser UI keeps its chat list in `localStorage`; conversations opted into server-side tracking (via a document attachment, or an API caller supplying `conversation_id`) are additionally persisted in embedded SQLite so later turns don't need to resend full history.
- Blue/white UI built with a real Tailwind CSS build and self-hosted Font Awesome 4 (see `webui-src/`).
- A multi-stage, non-root container image (adds a Node stage to build the UI's CSS/icons).
- Terraform for required APIs (including Cloud Build), Artifact Registry, a dedicated service account, a least-privilege custom role, its IAM binding, a standard GKE cluster, Fleet registration, and Managed Cloud Service Mesh.
- Kubernetes manifests (`k8s/deployment.yaml`) for the Deployment (1 replica — see [Deploy to GKE](#deploy-to-gke)), a PersistentVolumeClaim for the SQLite database, the ClusterIP Service, and the Workload Identity-bound ServiceAccount.
- Health endpoint at `GET /healthz` (unauthenticated, for liveness/readiness probes).
- Embedded Swagger UI at `GET /docs/` and its OpenAPI 3.1 document at `GET /openapi.json` (both require login, like the rest of the app).

## Authentication

There are two, unrelated layers of authentication in this app — don't conflate them:

1. **[App authentication](#app-authentication-static-token)** — a single shared token that gates the app itself (both the web UI and the API). This is what an end user or an API caller needs.
2. **[GCP authentication](#gcp-authentication-workload-identity)** — how the *server* talks to Vertex AI. This is infrastructure-level and end users never see it.

### App authentication (static token)

Every route except `POST /api/login` and `GET /healthz` requires the shared `AUTH_TOKEN` configured on the server, via **one of**:

- `Authorization: Bearer <AUTH_TOKEN>` header — for API callers (curl, SDKs, scripts). This is the same header shape a real OpenAI API key would use, so most OpenAI-compatible clients need no special handling.
- The `vc_auth` cookie — for the browser UI, set automatically after submitting the token on the login screen (`POST /api/login`). The cookie never carries the raw token, only an HMAC derived from it, so rotating `AUTH_TOKEN` invalidates every existing login immediately.

Generate a strong token and set it as `AUTH_TOKEN` (see [Configuration](#configuration)). On GKE, it's supplied via a Secret, never a plain env value in the manifest — see [Deploy to GKE](#deploy-to-gke).

### GCP authentication (Workload Identity)

The pod runs as a Kubernetes ServiceAccount (`vertex-chat-ksa`, see `k8s/deployment.yaml`) bound to the Terraform-managed Google service account via **GKE Workload Identity Federation**. The Google SDK obtains short-lived credentials automatically through the cluster's metadata server — the same ADC mechanism used everywhere else on Google Cloud, just scoped per-pod instead of per-VM. Do not create a service-account key and do not set `GOOGLE_APPLICATION_CREDENTIALS`.

The binding (`roles/iam.workloadIdentityUser`, scoped to `default/vertex-chat-ksa`) is created by Terraform — `google_service_account_iam_member.vertex_gemini_workload_identity` in `main.tf`. If the namespace or ServiceAccount name in `k8s/deployment.yaml` changes, update `gke_workload_identity_namespace` / `gke_workload_identity_ksa` in `terraform.tfvars` to match.

The custom role always contains:

```text
aiplatform.endpoints.predict
```

The existing Terraform switches can optionally add model discovery or GCS-read permissions. They default to `false` because the current text-chat application does not need them.

## Local development

### Mock mode

Mock mode exercises the full UI and streaming API without GCP credentials or model charges. It sets a fixed local `AUTH_TOKEN` (`dev-local-token`) so login works out of the box:

```bash
make run-mock
```

Open [http://localhost:8080](http://localhost:8080) and log in with `dev-local-token`.

Interactive API documentation is available at [http://localhost:8080/docs/](http://localhost:8080/docs/) (also requires login — send the same token as a Bearer header, or use the cookie the browser already has after logging in).

Test the streaming endpoint directly:

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dev-local-token' \
  -d '{
    "model": "gemini-3.5-flash",
    "stream": true,
    "messages": [
      {"role": "system", "content": "Be concise."},
      {"role": "user", "content": "Explain Kubernetes in one sentence."}
    ]
  }'
```

### Live Vertex AI mode

Create local ADC credentials and set the project:

```bash
gcloud auth application-default login
gcloud config set project YOUR_PROJECT_ID

GOOGLE_CLOUD_PROJECT=YOUR_PROJECT_ID \
GOOGLE_CLOUD_LOCATION=global \
MODEL_ID=gemini-3.5-flash \
AUTH_TOKEN=$(openssl rand -base64 32) \
go run ./cmd/server
```

Your local identity needs `aiplatform.endpoints.predict` in the selected project. The generated `AUTH_TOKEN` is printed nowhere on purpose — export it to a variable first if you want to reuse it:

```bash
export AUTH_TOKEN=$(openssl rand -base64 32)
echo "$AUTH_TOKEN"   # save this to log in
GOOGLE_CLOUD_PROJECT=YOUR_PROJECT_ID GOOGLE_CLOUD_LOCATION=global MODEL_ID=gemini-3.5-flash go run ./cmd/server
```

### Quality checks

```bash
go test -race ./...
go vet ./...
terraform fmt -check -recursive
terraform validate
docker build -t vertex-chat:local .
```

## API contract

Supported request fields are:

- `model` (defaults to the configured model and must match it)
- `messages` with `system`, `developer`, `user`, and `assistant` roles and string content
- `stream`
- `temperature`
- `top_p`
- `max_tokens`
- `stop` as a string or string array
- `n` when omitted or set to `1`
- `conversation_id` (optional, not part of the OpenAI spec — see below)

Streaming responses use `text/event-stream`, OpenAI-style `chat.completion.chunk` objects, and terminate with `data: [DONE]`. Unsupported OpenAI features such as tool calls, multimodal content arrays, log probabilities, multiple candidates, and JSON response schemas return an `invalid_request_error` instead of being silently ignored.

## Using the OpenAI-compatible endpoint

`POST /v1/chat/completions` follows the OpenAI Chat Completions request/response shape closely enough that the official `openai` client libraries work against it unmodified — just point `base_url` at this server and pass `AUTH_TOKEN` as the API key. This works from **any language or tool** that can send an HTTP request with a JSON body and a Bearer header; the SDK examples below are conveniences, not a requirement.

### Authentication

Every call needs:

```
Authorization: Bearer <AUTH_TOKEN>
```

There's no separate "API key" — it's the same shared token used to log into the web UI (see [App authentication](#app-authentication-static-token)). A missing or wrong token returns `401` with an OpenAI-style error body:

```json
{"error": {"message": "authentication required", "type": "authentication_error", "param": null, "code": "unauthorized"}}
```

### curl

Non-streaming:

```bash
curl https://your-vertex-chat-host/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"role": "system", "content": "Be concise."},
      {"role": "user", "content": "Explain Kubernetes in one sentence."}
    ]
  }'
```

`model` can be omitted (it defaults to the server's configured model) or must exactly match it — sending a different model name returns a `400`.

Streaming (`-N` disables curl's output buffering so chunks print as they arrive):

```bash
curl -N https://your-vertex-chat-host/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "stream": true,
    "messages": [{"role": "user", "content": "Write a haiku about deployment pipelines."}]
  }'
```

### Python (`openai` SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://your-vertex-chat-host/v1",
    api_key="YOUR_AUTH_TOKEN",  # the same shared token, not a real OpenAI key
)

response = client.chat.completions.create(
    model="gemini-3.5-flash",
    messages=[
        {"role": "system", "content": "Be concise."},
        {"role": "user", "content": "Explain Kubernetes in one sentence."},
    ],
)
print(response.choices[0].message.content)
```

Streaming:

```python
stream = client.chat.completions.create(
    model="gemini-3.5-flash",
    messages=[{"role": "user", "content": "Write a haiku about deployment pipelines."}],
    stream=True,
)
for chunk in stream:
    delta = chunk.choices[0].delta.content
    if delta:
        print(delta, end="", flush=True)
```

### Node.js / TypeScript (`openai` SDK)

```bash
npm install openai
```

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "https://your-vertex-chat-host/v1",
  apiKey: "YOUR_AUTH_TOKEN", // the same shared token, not a real OpenAI key
});

const response = await client.chat.completions.create({
  model: "gemini-3.5-flash",
  messages: [
    { role: "system", content: "Be concise." },
    { role: "user", content: "Explain Kubernetes in one sentence." },
  ],
});
console.log(response.choices[0].message.content);
```

Streaming:

```javascript
const stream = await client.chat.completions.create({
  model: "gemini-3.5-flash",
  messages: [{ role: "user", content: "Write a haiku about deployment pipelines." }],
  stream: true,
});
for await (const chunk of stream) {
  const delta = chunk.choices[0]?.delta?.content;
  if (delta) process.stdout.write(delta);
}
```

### Any other language/framework

The pattern is the same everywhere: send `POST <base-url>/v1/chat/completions` with `Content-Type: application/json`, an `Authorization: Bearer <AUTH_TOKEN>` header, and a JSON body with a `messages` array. Any HTTP client (Go's `net/http`, Java's `HttpClient`, Ruby's `net/http`, LangChain's `ChatOpenAI` with a custom `base_url`, etc.) works without a dedicated SDK.

### Server-persisted conversations (`conversation_id`)

By default the endpoint is **fully stateless** — you resend the whole message history (including any `system` message) on every call, exactly like the real OpenAI API, and the server stores nothing. This is the original, backward-compatible behavior and needs no extra fields.

Optionally, set `conversation_id` to a string you generate (a UUID, a request ID, anything opaque) to have the server persist and replay history for you:

- **First call with a new id**: send the full first turn as usual. The server creates a conversation under that exact id.
- **Later calls with the same id**: send only the *new* turn(s) — the server prepends everything already stored (plus the extracted text of any documents uploaded to that conversation via the web UI's attach button or `POST /api/conversations/{id}/documents`) before calling Gemini. You still don't need to resend `system` — it's read fresh from your request each time and never gets duplicated.
- The response echoes back `conversation_id` so you can confirm which thread you're on.

```bash
curl https://your-vertex-chat-host/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "My name is Priya."}],
    "conversation_id": "session-42"
  }'

# Later, same id — the server already remembers "Priya":
curl https://your-vertex-chat-host/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is my name?"}],
    "conversation_id": "session-42"
  }'
```

This isn't a standard OpenAI field, so real OpenAI clients simply never send it and get the original stateless behavior; it's additive and safe to ignore if you don't need it.

## Deploy to GKE

The first apply creates the required APIs, IAM resources, Artifact Registry repository, the GKE cluster, its Fleet membership, and Managed Cloud Service Mesh (see `gke.tf`).

### 1. Configure and bootstrap infrastructure

```bash
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars and set project_id.

terraform init
terraform plan -out=tfplan
terraform apply tfplan
```

The Terraform executor needs permission to enable APIs, create service accounts and custom roles, update project IAM, create Artifact Registry repositories, and create GKE clusters and Fleet memberships. GKE cluster creation takes several minutes; Cloud Service Mesh provisioning (`MANAGEMENT_AUTOMATIC`) takes about 10 more — check status with `gcloud container fleet mesh describe`.

### 2. Build and push the image

```bash
PROJECT_ID="YOUR_PROJECT_ID"
REGION="asia-south1"
IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/vertex-chat/vertex-chat:v1"

gcloud auth configure-docker "${REGION}-docker.pkg.dev"
gcloud builds submit --tag "${IMAGE}" .
```

Cloud Build requires its executing identity to have permission to build and push to the repository. A local Docker build and push can be used instead.

### 3. Deploy

The Workload Identity binding is already created by Terraform (see [GCP authentication](#gcp-authentication-workload-identity)) — just fetch cluster credentials:

```bash
gcloud container clusters get-credentials "$(terraform output -raw gke_cluster_name)" \
  --zone "$GKE_LOCATION" --project "$PROJECT_ID"
```

Create the `AUTH_TOKEN` secret **before the first apply** — `k8s/deployment.yaml` references it via `secretKeyRef` and never contains the token itself:

```bash
kubectl create secret generic vertex-chat-auth --namespace default \
  --from-literal=token="$(openssl rand -base64 32)"
```

Update the `image:` and `GOOGLE_CLOUD_PROJECT` values in `k8s/deployment.yaml` (they default to `YOUR_PROJECT_ID` placeholders — the `deploy.yml` workflow does this substitution automatically if you deploy via GitHub Actions instead), then apply it:

```bash
kubectl apply -f k8s/deployment.yaml
kubectl rollout status deployment/vertex-chat
```

The `Service` is `type: ClusterIP` — it's reached through the Istio ingress gateway configured in `k8s/ingressgateway.yaml`, not a public IP of its own. Point DNS at that gateway's IP, or add the `LoadBalancer`/Ingress + Cloud Armor of your choice in front. Since `AUTH_TOKEN` already gates the whole app (see [App authentication](#app-authentication-static-token)), exposure of the gateway itself is less sensitive than an unauthenticated app would be — but it's still worth restricting network-level access for production use.

The Deployment runs a **single replica** with a PersistentVolumeClaim (`vertex-chat-data`) backing the embedded SQLite database. This isn't just a cost choice — SQLite needs single-writer access, and a `ReadWriteOnce` PVC can only attach to one node's pod at a time, so running more than one replica isn't supported without swapping in a real network database first.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listening port |
| `GOOGLE_CLOUD_PROJECT` | required unless `DEV_MOCK=true` | Vertex AI project |
| `GOOGLE_CLOUD_LOCATION` | `global` | Vertex AI endpoint location |
| `MODEL_ID` | `gemini-3.5-flash` | Allowed model ID |
| `DEV_MOCK` | `false` | Use the local deterministic streaming model and document analyzer |
| `AUTH_TOKEN` | required | Shared token gating the UI and API — see [App authentication](#app-authentication-static-token) |
| `DB_PATH` | `./data/vertex-chat.db` | SQLite database file path |
| `COOKIE_SECURE` | `true` | `Secure` flag on cookies; set `false` for plain-HTTP local dev |
| `MAX_REQUEST_BYTES` | `2097152` | Maximum JSON request body for `/v1/chat/completions` |
| `MAX_MESSAGES` | `100` | Maximum messages per API request |
| `MAX_CONCURRENT_CHATS` | `32` | Per-instance generation limit (also gates document analysis) |
| `REQUEST_TIMEOUT` | `5m` | Maximum model generation / document analysis duration |
| `ALLOWED_ORIGINS` | empty | Comma-separated CORS origins; same-origin UI needs none |
| `MAX_UPLOAD_BYTES` | `20971520` (20 MiB) | Maximum uploaded document size |
| `MAX_DOCUMENT_CONTEXT_CHARS` | `24000` | Maximum characters of extracted document text injected per request |
| `MAX_CONTEXT_MESSAGES` | `40` | Maximum stored messages replayed per `conversation_id` |
| `MAX_CONVERSATIONS_PER_SESSION` | `200` | Per-browser-session conversation cap |
| `MAX_DOCUMENTS_PER_CONVERSATION` | `20` | Per-conversation document cap |
| `MAX_SAVED_PROMPTS_PER_SESSION` | `100` | Per-browser-session saved-prompt cap |

## Repository layout

```text
cmd/server/             HTTP server entry point
internal/api/           OpenAI-compatible + companion HTTP/SSE adapter
                        (chat, auth, conversations, documents, prompts)
internal/auth/          Static shared-token cookie/header verification
internal/session/       Anonymous per-browser session cookie
internal/store/         SQLite schema and CRUD (conversations, messages,
                        documents, saved prompts)
internal/chat/          Provider-neutral chat/document-analysis contracts
                        and local mocks
internal/config/        Environment configuration
internal/vertex/        Google Gen AI SDK / Vertex AI adapter
internal/webui/assets/  Embedded browser application (HTML/JS, built
                        Tailwind CSS, self-hosted Font Awesome 4)
webui-src/              Tailwind CSS + Font Awesome 4 build pipeline for
                        internal/webui/assets/ (Node, not needed for
                        `go build`/`go test` — see Dockerfile)
k8s/deployment.yaml     GKE Deployment (1 replica), PersistentVolumeClaim,
                        ServiceAccount, and Service (ClusterIP)
k8s/ingressgateway.yaml Istio Gateway/VirtualService the app is reached through
main.tf                 All Terraform: APIs, Artifact Registry, service account,
                        custom role, IAM bindings, GKE cluster, Fleet, and mesh
.github/workflows/      CI, Docker publish, and deploy pipelines (see below)
```

## CI/CD (GitHub Actions)

Three workflows, all under `.github/workflows/`:

| Workflow | Trigger | Does |
| --- | --- | --- |
| `ci.yml` | push/PR to `main` | `go test` + coverage, and a build + `govulncheck` scan — both publish results to the job Summary tab |
| `docker-publish.yml` | manual (`workflow_dispatch`) | Builds and pushes a multi-arch image to `ghcr.io/mevijays/<repo>`; the Summary includes a digest-pinned image reference to hand to the deploy workflow |
| `deploy.yml` | manual (`workflow_dispatch`) | Authenticates to GCP via Workload Identity Federation (no keys), fetches GKE credentials, and applies `k8s/deployment.yaml` with the image you supply |

`deploy.yml` needs two repository variables (**Settings → Secrets and variables → Actions → Variables**), taken from `terraform output` after applying `main.tf`:

```bash
terraform output -raw github_actions_workload_identity_provider   # -> GCP_WORKLOAD_IDENTITY_PROVIDER
terraform output -raw github_actions_deploy_service_account       # -> GCP_DEPLOY_SERVICE_ACCOUNT
```

Before applying, set `github_repository` in `terraform.tfvars` to the exact `owner/repo` this code is pushed to — it's the attribute condition that restricts which GitHub repository is allowed to assume the deploy identity.
