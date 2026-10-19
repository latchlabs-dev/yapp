import { useCallback, useEffect, useRef, useState } from "react";
import { sendMessage } from "@/lib/api";
import { readEventStream } from "@/lib/stream";
import { applyTurnEvent, newTurn, type LiveTurn, type SendInput } from "@/lib/turn";

export type { LiveTurn, SendInput, SentAttachment, TurnStatus } from "@/lib/turn";

const RELOAD_FAILED =
  "The conversation could not be reloaded, so this reply is shown as it streamed rather than as it was stored.";

/**
 * useTurn drives conversation turns over the event stream.
 *
 * Turns are keyed by conversation. React keeps this component mounted when only
 * the `:conversationId` param changes, so a single shared turn would render
 * Alpha's reply — and Alpha's Stop button — inside Beta. Every state update is
 * therefore addressed to the conversation and the turn token it belongs to, and
 * a stream that outlives the switch keeps streaming under the conversation that
 * started it.
 *
 * A turn's provisional state is cleared only once the reload that replaces it
 * has resolved. A failed turn is never cleared at all: its error and its input
 * are the only record the user has of what happened, and it carries a retry.
 */
export function useTurn(reload: (conversationId: string) => Promise<unknown>) {
  const [turns, setTurns] = useState<Record<string, LiveTurn>>({});
  const controllers = useRef<Record<string, AbortController>>({});
  const nextToken = useRef(0);
  const reloadRef = useRef(reload);

  useEffect(() => {
    reloadRef.current = reload;
  }, [reload]);

  // Abandoned streams are aborted when the chat view goes away. Leaving them
  // running would spend tokens on a reply nothing will ever render.
  useEffect(() => {
    const live = controllers.current;
    return () => {
      for (const controller of Object.values(live)) controller.abort();
    };
  }, []);

  /** patch updates one turn, ignoring events from a turn that was superseded. */
  const patch = useCallback(
    (conversationId: string, token: number, update: (turn: LiveTurn) => LiveTurn) => {
      setTurns((current) => {
        const turn = current[conversationId];
        if (!turn || turn.token !== token) return current;
        return { ...current, [conversationId]: update(turn) };
      });
    },
    [],
  );

  /**
   * settle reloads the conversation and then decides what remains on screen.
   * The reload comes first so the thread never blinks empty between the
   * provisional reply and the stored one.
   */
  const settle = useCallback(async (conversationId: string, token: number) => {
    let reloaded = true;
    try {
      await reloadRef.current(conversationId);
    } catch {
      reloaded = false;
    }
    setTurns((current) => {
      const turn = current[conversationId];
      if (!turn || turn.token !== token) return current;
      if (turn.error || turn.blocked) {
        return { ...current, [conversationId]: { ...turn, status: "failed" } };
      }
      if (!reloaded) {
        return {
          ...current,
          [conversationId]: { ...turn, status: "done", notices: [...turn.notices, RELOAD_FAILED] },
        };
      }
      const next = { ...current };
      delete next[conversationId];
      return next;
    });
  }, []);

  /**
   * send starts a turn and resolves as soon as the server has taken it.
   *
   * Acceptance and completion are deliberately different moments. The composer
   * waits on acceptance — that is when the prompt stops being a draft and
   * becomes a stored message — while the stream goes on filling the live turn
   * behind it. Resolving only at completion would leave the sent text sitting
   * in the composer for the whole reply.
   */
  const send = useCallback(
    (conversationId: string, input: SendInput): Promise<boolean> => {
      // A second send in the same conversation replaces the first.
      controllers.current[conversationId]?.abort();
      const controller = new AbortController();
      controllers.current[conversationId] = controller;
      const token = ++nextToken.current;
      setTurns((current) => ({
        ...current,
        [conversationId]: newTurn(conversationId, token, input),
      }));

      let settleAccepted: (accepted: boolean) => void = () => {};
      const accepted = new Promise<boolean>((resolve) => {
        settleAccepted = resolve;
      });

      void (async () => {
        try {
          const response = await sendMessage(
            conversationId,
            {
              content: input.content,
              attachment_ids: input.attachments.map((a) => a.id),
              model: input.model,
              provider_id: input.providerId,
            },
            controller.signal,
          );
          // A 200 means the prompt is stored: the handler persists the user
          // turn before it opens the stream, and every refusal before that
          // point is a non-2xx.
          patch(conversationId, token, (turn) => ({ ...turn, accepted: true }));
          settleAccepted(true);
          if (!response.body) throw new Error("the server returned no event stream");

          await readEventStream(
            response.body,
            (event) => patch(conversationId, token, (turn) => applyTurnEvent(turn, event)),
            controller.signal,
          );
        } catch (err) {
          if (!controller.signal.aborted) {
            patch(conversationId, token, (turn) => ({
              ...turn,
              error: err instanceof Error ? err.message : "the request failed",
            }));
          }
        } finally {
          // A no-op once the turn was accepted; the decisive call for a refusal.
          settleAccepted(false);
          if (controllers.current[conversationId] === controller) {
            delete controllers.current[conversationId];
          }
          if (!controller.signal.aborted) {
            await settle(conversationId, token);
          }
        }
      })();

      return accepted;
    },
    [patch, settle],
  );

  /**
   * cancel stops the turn for one conversation and reconciles with the server.
   *
   * The prompt was persisted before the stream opened, so it survives the stop
   * even though the reply does not. Reloading is what makes it visible; without
   * it the conversation looks as though nothing was ever sent.
   */
  const cancel = useCallback((conversationId: string) => {
    controllers.current[conversationId]?.abort();
    delete controllers.current[conversationId];
    setTurns((current) => {
      const turn = current[conversationId];
      if (!turn || turn.status !== "streaming") return current;
      return { ...current, [conversationId]: { ...turn, status: "stopped" } };
    });
    void reloadRef.current(conversationId).catch(() => {});
  }, []);

  /** dismiss drops a finished or failed turn the user has read. */
  const dismiss = useCallback((conversationId: string) => {
    setTurns((current) => {
      if (!current[conversationId]) return current;
      const next = { ...current };
      delete next[conversationId];
      return next;
    });
  }, []);

  const turnFor = useCallback(
    (conversationId: string | undefined): LiveTurn | null =>
      (conversationId && turns[conversationId]) || null,
    [turns],
  );

  const isSending = useCallback(
    (conversationId: string | undefined): boolean =>
      !!conversationId && turns[conversationId]?.status === "streaming",
    [turns],
  );

  return { turnFor, isSending, send, cancel, dismiss };
}
