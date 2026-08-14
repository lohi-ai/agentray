package channels

func init() {
	// Shipped adapters — ingress already lives in internal/app and
	// internal/runtime. This catalog is the extension point; a new adapter
	// registers here the same way.
	Register(Info{Kind: KindChat, Mode: ModeSync, Description: "In-app streaming chat with a Garden agent"})
	Register(Info{Kind: KindMCP, Mode: ModeTools, Description: "MCP server projecting opcore operations to external agents"})
	Register(Info{Kind: KindSchedule, Mode: ModeAsync, Description: "Cron trigger on a Garden agent"})
	Register(Info{Kind: KindWebhook, Mode: ModeAsync, Description: "Authenticated inbound HTTP hook on a Garden agent"})
	Register(Info{Kind: KindLab, Mode: ModeSync, Description: "Manual / Lab run from AgentGarden"})
	// Reserved: do not mount ingress until an adapter exists.
	Register(Info{Kind: KindSupportWidget, Mode: ModeSync, Reserved: true, Description: "Embedded support widget (reserved)"})
	Register(Info{Kind: KindVoice, Mode: ModeSync, Reserved: true, Description: "Voice / call adapter (reserved)"})
}
