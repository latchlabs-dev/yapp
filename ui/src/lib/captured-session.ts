export interface CapturedSessionRef {
  conversation_id?: string;
  tenant_id?: string;
  user_id?: string;
  owner_id?: string;
  transcript_state?: string;
  from: string;
  to: string;
}

// Defend the rendered scope even if an older API ignores session_scope.
export function belongsToCapturedSession(payload: {
  conversation_id?: string; tenant_id?: string; user_id?: string;
  key_id?: string; created_at: string;
}, session: CapturedSessionRef): boolean {
  const owner = payload.user_id ? `user:${payload.user_id}` : payload.key_id ? `key:${payload.key_id}` : "";
  const selectedOwner = session.owner_id || (session.user_id ? `user:${session.user_id}` : "");
  const created = Date.parse(payload.created_at);
  return (payload.conversation_id || "") === (session.conversation_id || "")
    && (payload.tenant_id || "") === (session.tenant_id || "")
    && owner === selectedOwner
    && created >= Date.parse(session.from) && created <= Date.parse(session.to);
}
