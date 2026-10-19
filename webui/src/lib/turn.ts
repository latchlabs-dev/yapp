import type { StartEvent, TokenUsage, TurnEvent } from "./stream";

// The state of one conversation turn and the fold that advances it. Kept
// free of React so the stream vocabulary can be unit-tested directly;
// hooks/use-turn.ts owns the requests, the abort controllers and the
// per-conversation bookkeeping.

/** One attachment as submitted, kept so a failed turn can be retried verbatim. */
export interface SentAttachment {
  id: string;
  filename: string;
}

export interface SendInput {
  content: string;
  attachments: SentAttachment[];
  model?: string;
  providerId?: string;
}

export type TurnStatus =
  /** In flight: the reply is still arriving. */
  | "streaming"
  /** The user pressed Stop; whatever arrived is kept and labelled. */
  | "stopped"
  /** The send was refused, or the stream reported an error or a hook refusal. */
  | "failed"
  /** Finished, but still on screen because the thread could not be reloaded. */
  | "done";

/** The live state of one in-flight turn, rendered as a provisional message. */
export interface LiveTurn {
  conversationId: string;
  /** Distinguishes this turn from a later one in the same conversation. */
  token: number;
  /** Exactly what was submitted. Retry resends this; nothing is re-typed. */
  input: SendInput;
  status: TurnStatus;
  /**
   * True once the server answered 200, which is also when it has persisted the
   * prompt. Below that line the draft is still the only copy and belongs back
   * in the composer; above it the prompt is in the conversation and the turn
   * owns it.
   */
  accepted: boolean;
  text: string;
  reasoning: string;
  usage?: TokenUsage;
  /** Set when a hook refused the turn; rendered differently from an error. */
  blocked?: string;
  error?: string;
  notices: string[];
  annotations: Record<string, string>;
  start?: StartEvent;
}

/** newTurn is the state a turn starts in, before the server has answered. */
export function newTurn(conversationId: string, token: number, input: SendInput): LiveTurn {
  return {
    conversationId,
    token,
    input,
    status: "streaming",
    accepted: false,
    text: "",
    reasoning: "",
    notices: [],
    annotations: {},
  };
}

/**
 * applyTurnEvent folds one stream event into the live turn. Pure, so the
 * stream vocabulary can be tested without React.
 */
export function applyTurnEvent(turn: LiveTurn, event: TurnEvent): LiveTurn {
  const next: LiveTurn = { ...turn };
  switch (event.type) {
    case "start":
      next.start = event.data;
      break;
    case "delta":
      next.text += event.data.text;
      break;
    case "reasoning":
      next.reasoning += event.data.text;
      break;
    case "usage":
      next.usage = event.data;
      break;
    case "notice":
      if (event.data.message) next.notices = [...next.notices, event.data.message];
      if (event.data.annotations) {
        next.annotations = { ...next.annotations, ...event.data.annotations };
      }
      break;
    case "blocked":
      next.blocked = event.data.message;
      break;
    case "error":
      next.error = event.data.message;
      break;
    case "done":
      break;
  }
  return next;
}

