import type { LiveTurn } from "./turn";

// What a screen reader is told about a turn. A streaming reply, a refusal and a
// completion are all silent visual changes otherwise: the text simply appears,
// or fails to, somewhere the reader is not focused.

export interface Announcement {
  /** Empty when there is nothing to say, so the live region stays quiet. */
  text: string;
  /** Errors interrupt; progress waits for a pause. */
  assertive: boolean;
}

export function announceTurn(turn: LiveTurn | null, threadLoading: boolean): Announcement {
  if (threadLoading) return { text: "Loading the conversation.", assertive: false };
  if (!turn) return { text: "", assertive: false };

  if (turn.error) return { text: `The turn failed: ${turn.error}`, assertive: true };
  if (turn.blocked) return { text: `The turn was refused: ${turn.blocked}`, assertive: true };

  switch (turn.status) {
    case "streaming":
      // Two distinct states: waiting on the model, and receiving its answer.
      // Announcing token counts or the text itself would flood the reader.
      return turn.text
        ? { text: "The reply is arriving.", assertive: false }
        : { text: "Message sent. Waiting for the reply.", assertive: false };
    case "stopped":
      return { text: "You stopped the reply.", assertive: false };
    case "done":
      return { text: "The reply is complete.", assertive: false };
    default:
      return { text: "", assertive: false };
  }
}
