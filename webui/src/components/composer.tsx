import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { ArrowUp, Paperclip, Square } from "lucide-react";
import { toast } from "sonner";
import { uploadAttachment } from "@/lib/api";
import { checkAttachment } from "@/lib/attachments";
import type { Model } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { PendingAttachmentChip } from "@/components/message-item";
import { cn } from "@/lib/utils";

export interface PendingAttachment {
  key: string;
  id?: string;
  filename: string;
  sizeBytes: number;
  uploading: boolean;
}

/** One conversation's unsent message. Owned by the page, keyed by conversation. */
export interface Draft {
  text: string;
  attachments: PendingAttachment[];
}

export const emptyDraft: Draft = { text: "", attachments: [] };

interface ComposerProps {
  disabled: boolean;
  sending: boolean;
  /** False while a send would be refused: no model, mid-send, mid-upload, empty. */
  canSend: boolean;
  model: Model | undefined;
  maxAttachmentBytes: number;
  attachmentsEnabled: boolean;
  conversationId?: string;
  /** The draft for the conversation on screen; never shared between threads. */
  draft: Draft;
  onDraftChange: (update: (draft: Draft) => Draft) => void;
  /**
   * Sends the current draft. The page owns both the draft and the send, so a
   * retry offered elsewhere in the thread submits exactly what this composer
   * holds rather than a second, stale copy of it.
   */
  onSubmit: () => void;
  onCancel: () => void;
  /** Per-thread settings (model, project) shown in the composer's control row. */
  controls?: ReactNode;
}

export function Composer({
  disabled,
  sending,
  canSend,
  model,
  maxAttachmentBytes,
  attachmentsEnabled,
  conversationId,
  draft,
  onDraftChange,
  onSubmit,
  onCancel,
  controls,
}: ComposerProps) {
  const [dragging, setDragging] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const textarea = useRef<HTMLTextAreaElement>(null);

  const { text, attachments } = draft;

  // The draft is owned by the page, so switching conversations changes `text`
  // without a keystroke. Resize from the value rather than from the event.
  useEffect(() => {
    const el = textarea.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = text ? `${Math.min(el.scrollHeight, 240)}px` : "auto";
  }, [text]);

  const addFiles = useCallback(
    async (files: FileList | File[]) => {
      for (const file of Array.from(files)) {
        // Check before uploading: a user should learn that a model cannot take
        // an image before spending the upload, not after.
        const verdict = checkAttachment(file, model, maxAttachmentBytes);
        if (!verdict.ok) {
          toast.error(verdict.reason);
          continue;
        }
        const key = `${file.name}-${file.size}-${Date.now()}-${Math.random()}`;
        onDraftChange((current) => ({
          ...current,
          attachments: [
            ...current.attachments,
            { key, filename: file.name, sizeBytes: file.size, uploading: true },
          ],
        }));
        try {
          const attachment = await uploadAttachment(file, conversationId);
          onDraftChange((current) => ({
            ...current,
            attachments: current.attachments.map((p) =>
              p.key === key ? { ...p, id: attachment.id, uploading: false } : p,
            ),
          }));
        } catch (err) {
          onDraftChange((current) => ({
            ...current,
            attachments: current.attachments.filter((p) => p.key !== key),
          }));
          toast.error(err instanceof Error ? err.message : `Could not upload ${file.name}`);
        }
      }
    },
    [conversationId, maxAttachmentBytes, model, onDraftChange],
  );

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    // Enter sends, Shift+Enter breaks the line — the convention every chat
    // client uses, and the one people's fingers already know.
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      if (canSend) onSubmit();
    }
  };

  const onDrop = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    setDragging(false);
    if (attachmentsEnabled && event.dataTransfer.files.length > 0) {
      void addFiles(event.dataTransfer.files);
    }
  };

  return (
    <div
      onDragOver={(e) => {
        e.preventDefault();
        if (attachmentsEnabled) setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={onDrop}
      className={cn(
        "rounded-2xl border bg-card shadow-lg shadow-black/20 transition-colors focus-within:border-primary/40",
        dragging ? "border-primary bg-primary/5" : "border-border",
      )}
    >
      {attachments.length > 0 && (
        <ul className="flex flex-wrap gap-1.5 px-3 pt-3">
          {attachments.map((p) => (
            <li key={p.key} className="min-w-0 max-w-full">
              <PendingAttachmentChip
                filename={p.filename}
                sizeBytes={p.sizeBytes}
                uploading={p.uploading}
                onRemove={() =>
                  onDraftChange((current) => ({
                    ...current,
                    attachments: current.attachments.filter((c) => c.key !== p.key),
                  }))
                }
              />
            </li>
          ))}
        </ul>
      )}

      <textarea
        ref={textarea}
        value={text}
        onChange={(e) => {
          const value = e.target.value;
          onDraftChange((current) => ({ ...current, text: value }));
        }}
        onKeyDown={onKeyDown}
        disabled={disabled}
        rows={1}
        placeholder={disabled ? "Select a model to start" : "Ask anything…"}
        aria-label="Message"
        className="block max-h-60 min-h-12 w-full resize-none bg-transparent px-4 pb-1 pt-3.5 text-[15px] leading-6 outline-none placeholder:text-muted-foreground/70 disabled:opacity-50"
      />

      <div className="flex items-center gap-1 px-2 pb-2">
        {attachmentsEnabled && (
          <>
            <input
              ref={fileInput}
              type="file"
              multiple
              hidden
              onChange={(e) => {
                if (e.target.files) void addFiles(e.target.files);
                e.target.value = "";
              }}
            />
            <Button
              variant="ghost"
              size="sm"
              disabled={disabled}
              onClick={() => fileInput.current?.click()}
              aria-label="Attach files"
              className="h-8 w-8 shrink-0 rounded-lg p-0 text-muted-foreground hover:text-foreground"
            >
              <Paperclip className="h-4 w-4" />
            </Button>
          </>
        )}

        {/* min-w-0 lets the pickers shrink on a phone instead of pushing the
            send button off the edge. */}
        <div className="flex min-w-0 flex-1 items-center gap-1">{controls}</div>

        {sending ? (
          <Button
            variant="secondary"
            size="sm"
            onClick={onCancel}
            className="h-8 shrink-0 rounded-lg"
          >
            <Square className="mr-1.5 h-3 w-3 fill-current" />
            Stop
          </Button>
        ) : (
          <Button
            size="sm"
            disabled={!canSend}
            onClick={onSubmit}
            className="h-8 w-8 shrink-0 rounded-lg p-0"
            aria-label="Send"
          >
            <ArrowUp className="h-4 w-4" />
          </Button>
        )}
      </div>
    </div>
  );
}
