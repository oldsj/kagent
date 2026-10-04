package env

var ChatGPTRefreshImage = RegisterStringVar(
	"KAGENT_CHATGPT_REFRESH_IMAGE", "",
	"Codex Harness image containing pinned Codex 0.148.0 and kagent-credential-refresh. Empty disables scheduled credential Jobs.",
	ComponentController,
)
