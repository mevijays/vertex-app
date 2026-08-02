# Vertex Chat

Vertex Chat is a Go web application for Gemini 3.5 Flash on Google Cloud. It provides a responsive browser chat interface and an OpenAI-compatible Chat Completions endpoint, streams model output with Server-Sent Events (SSE), and deploys as a keyless container on GKE.

## What is included

- `POST /v1/chat/completions` with streamed and non-streamed OpenAI-style responses.
- Gemini 3.5 Flash through the Google Gen AI SDK and Vertex AI.
- Multiple browser-local conversations, context replay, configurable system instructions, temperature, regeneration, cancellation, and light/dark themes.
- A multi-stage, non-root container image.
- Terraform for required APIs (including Cloud Build), Artifact Registry, a dedicated service account, a least-privilege custom role, its IAM binding, a standard GKE cluster, Fleet registration, and Managed Cloud Service Mesh.
- Kubernetes manifests (`k8s/deployment.yaml`) for the Deployment, Service (`LoadBalancer`), and Workload Identity-bound ServiceAccount.
- Health endpoint at `GET /healthz`.
- Embedded Swagger UI at `GET /docs/` and its OpenAPI 3.1 document at `GET /openapi.json`.

Conversation history is stored only in browser `localStorage`. The latest 40 turns are sent with each request as context. This is conversational memory, not model training or fine-tuning, and it avoids adding database permissions to the runtime service account.

## Authentication model

The pod runs as a Kubernetes ServiceAccount (`vertex-chat-ksa`, see `k8s/deployment.yaml`) bound to the Terraform-managed Google service account via **GKE Workload Identity Federation**. The Google SDK obtains short-lived credentials automatically through the cluster's metadata server — the same ADC mechanism used everywhere else on Google Cloud, just scoped per-pod instead of per-VM. Do not create a service-account key and do not set `GOOGLE_APPLICATION_CREDENTIALS`.

The binding (`roles/iam.workloadIdentityUser`, scoped to `default/vertex-chat-ksa`) is created by Terraform — `google_service_account_iam_member.vertex_gemini_workload_identity` in `main.tf`. If the namespace or ServiceAccount name in `k8s/deployment.yaml` changes, update `gke_workload_identity_namespace` / `gke_workload_identity_ksa` in `terraform.tfvars` to match.

The custom role always contains:

```text
aiplatform.endpoints.predict
```

The existing Terraform switches can optionally add model discovery or GCS-read permissions. They default to `false` because the current text-chat application does not need them.

## Local development

### Mock mode

Mock mode exercises the full UI and streaming API without GCP credentials or model charges:

```bash
make run-mock
```

Open [http://localhost:8080](http://localhost:8080).

Interactive API documentation is available at [http://localhost:8080/docs/](http://localhost:8080/docs/).

Test the streaming endpoint directly:

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
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
go run ./cmd/server
```

Your local identity needs `aiplatform.endpoints.predict` in the selected project.

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

Streaming responses use `text/event-stream`, OpenAI-style `chat.completion.chunk` objects, and terminate with `data: [DONE]`. Unsupported OpenAI features such as tool calls, multimodal content arrays, log probabilities, multiple candidates, and JSON response schemas return an `invalid_request_error` instead of being silently ignored.

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

The Workload Identity binding is already created by Terraform (see [Authentication model](#authentication-model)) — just fetch cluster credentials:

```bash
gcloud container clusters get-credentials "$(terraform output -raw gke_cluster_name)" \
  --zone us-central1-a --project "$PROJECT_ID"
```

Update the `image:` and `GOOGLE_CLOUD_PROJECT` values in `k8s/deployment.yaml` (they default to `YOUR_PROJECT_ID` placeholders), then apply it:

```bash
kubectl apply -f k8s/deployment.yaml
kubectl get service vertex-chat -w   # wait for EXTERNAL-IP
```

`k8s/deployment.yaml`'s `Service` is `type: LoadBalancer`, which provisions a public IP with no authentication in front of it by default — this is useful for a public chat experience but can permit untrusted usage and cost. For organizational or production use, put the application behind an Ingress + Cloud Armor, IAP, or add application-level sign-in instead.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listening port |
| `GOOGLE_CLOUD_PROJECT` | required | Vertex AI project |
| `GOOGLE_CLOUD_LOCATION` | `global` | Vertex AI endpoint location |
| `MODEL_ID` | `gemini-3.5-flash` | Allowed model ID |
| `DEV_MOCK` | `false` | Use the local deterministic streaming model |
| `MAX_REQUEST_BYTES` | `2097152` | Maximum JSON request body |
| `MAX_MESSAGES` | `100` | Maximum messages per API request |
| `MAX_CONCURRENT_CHATS` | `32` | Per-instance generation limit |
| `REQUEST_TIMEOUT` | `5m` | Maximum model generation duration |
| `ALLOWED_ORIGINS` | empty | Comma-separated CORS origins; same-origin UI needs none |

## Repository layout

```text
cmd/server/             HTTP server entry point
internal/api/           OpenAI-compatible HTTP/SSE adapter
internal/chat/          Provider-neutral chat contract and local mock
internal/config/        Environment configuration
internal/vertex/        Google Gen AI SDK / Vertex AI adapter
internal/webui/assets/  Embedded browser application
k8s/deployment.yaml     GKE Deployment, ServiceAccount, and Service (LoadBalancer)
main.tf                 All Terraform: APIs, Artifact Registry, service account,
                        custom role, IAM bindings, GKE cluster, Fleet, and mesh
```
