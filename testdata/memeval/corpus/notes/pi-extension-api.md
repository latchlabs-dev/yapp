# Pi extension API notes

The corp-proxy extension targets @earendil-works/pi-coding-agent. Verified API surface:
- Context injection: handle the before_agent_start event and return { systemPrompt: event.systemPrompt + extra }.
  This is the only supported way to extend the system prompt per turn.
- Custom tools: pi.registerTool({ name, label, description, parameters: Type.Object(...), execute() })
  with TypeBox schemas imported from "typebox". execute returns { content: [{type:"text", text}], details }.
- Session lifecycle: session_start, agent_start, agent_end, session_shutdown events.
- Auto-update: extension version is compared against /proxy/v1/extension-version; bump
  EXTENSION_VERSION, CurrentExtensionVersion, and the changelog together.
