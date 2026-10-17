package env

import "time"

var (
	SubstrateATEAPIEndpoint       = RegisterStringVar("KAGENT_SUBSTRATE_ATE_API_ENDPOINT", "dns:///api.ate-system.svc:443", "Substrate control-plane gRPC endpoint.", ComponentController)
	SubstrateATEAPICAFile         = RegisterStringVar("KAGENT_SUBSTRATE_ATE_API_CA_FILE", "", "PEM CA bundle used to verify the Substrate API server. Empty uses system trust roots.", ComponentController)
	SubstrateATEAPIClientCertFile = RegisterStringVar("KAGENT_SUBSTRATE_ATE_API_CLIENT_CERT_FILE", "", "PEM bundle containing both the client certificate and private key for Substrate API mTLS. Reloaded for each TLS handshake.", ComponentController)
)

var SubstrateAtenetRouterURL = RegisterStringVar(
	"KAGENT_SUBSTRATE_ATENET_ROUTER_URL",
	"http://atenet-router.ate-system.svc:80",
	"Substrate router endpoint for agent and sandbox guest traffic.",
	ComponentController,
)

var RuntimePayloadCatalog = RegisterStringVar("KAGENT_RUNTIME_PAYLOAD_CATALOG", "", "JSON provider/platform to {image, cliVersion} native payload catalog (e.g. claude/linux/arm64); images must be digest-pinned. Empty disables composed Sessions.", ComponentController)

var (
	SubstrateCallTimeout = RegisterDurationVar(
		"KAGENT_SUBSTRATE_CALL_TIMEOUT", 30*time.Second,
		"Deadline for each Substrate API call other than Actor pause, suspend, and resume. Must be positive.", ComponentController,
	)
	SubstrateLifecycleCallTimeout = RegisterDurationVar(
		"KAGENT_SUBSTRATE_LIFECYCLE_CALL_TIMEOUT", 5*time.Minute,
		"Deadline for each Substrate Actor pause, suspend, and resume call. Size it for the largest workspace snapshot at the observed storage rate; Substrate's own workflow deadline (ateApi --actor-workflow-deadline) must be at least as long. Must be positive and shorter than KAGENT_RUNTIME_OPERATION_TIMEOUT.", ComponentController,
	)
	RuntimeOperationTimeout = RegisterDurationVar(
		"KAGENT_RUNTIME_OPERATION_TIMEOUT", 10*time.Minute,
		"Deadline for one runtime lifecycle attempt, including its execution claim and any Substrate calls. An abandoned attempt can be retried after this bound. Must exceed KAGENT_SUBSTRATE_LIFECYCLE_CALL_TIMEOUT.", ComponentController,
	)
)
