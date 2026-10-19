import { useState } from "react";
import {
  BookmarkPlus,
  Check,
  FileText,
  Image as ImageIcon,
  Paperclip,
  ShieldAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/attachments";
import type { Message as ChatMessage, MessageBlock } from "@/lib/types";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import {
  Attachment,
  AttachmentAction,
  AttachmentActions,
  AttachmentContent,
  AttachmentDescription,
  AttachmentGroup,
  AttachmentMedia,
  AttachmentTitle,
  AttachmentTrigger,
} from "@/components/ui/attachment";
import { Bubble, BubbleContent } from "@/components/ui/bubble";
import { Message, MessageContent, MessageFooter } from "@/components/ui/message";
import { cn } from "@/lib/utils";

interface MessageItemProps {
  message: ChatMessage;
  conversationId: string;
  /** Enables "save to project memory"; off when the thread has no project. */
  canRemember: boolean;
}

export function MessageItem({ message, conversationId, canRemember }: MessageItemProps) {
  const [remembered, setRemembered] = useState(false);
  const [saving, setSaving] = useState(false);
  const isUser = message.role === "user";
  const blocks = message.content ?? [];
  const text = blocks
    .filter((b) => b.type === "text")
    .map((b) => b.text ?? "")
    .join("\n");
  const attachments = blocks.filter((b) => b.type === "attachment");

  const remember = async () => {
    setSaving(true);
    try {
      await api.remember(conversationId, message.id);
      setRemembered(true);
      toast.success("Saved to project memory");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Could not save to project memory");
    } finally {
      setSaving(false);
    }
  };

  // The user's turn is a bubble; the reply is plain prose across the full
  // column, the way a document reads. Boxing both made every exchange look
  // like two cards fighting for the same width.
  return (
    <Message align={isUser ? "end" : "start"}>
      <MessageContent className="gap-1.5">
        <Bubble
          variant={isUser ? "secondary" : "ghost"}
          align={isUser ? "end" : "start"}
          className={cn(isUser ? "max-w-[85%]" : "w-full")}
        >
          <BubbleContent
            className={cn(isUser ? "rounded-2xl rounded-br-md px-4 py-2.5" : "w-full overflow-visible")}
          >
            {text ? (
              <Markdown content={text} className={cn(!isUser && "assistant-prose space-y-4")} />
            ) : (
              <span className="text-sm italic text-muted-foreground">(no text)</span>
            )}

            {attachments.length > 0 && (
              <AttachmentGroup className="mt-2.5">
                {attachments.map((block) => (
                  <MessageAttachment key={block.attachment_id} block={block} />
                ))}
              </AttachmentGroup>
            )}
          </BubbleContent>
        </Bubble>

        <MessageFooter className="min-h-6 gap-2 px-0 text-[11px] font-normal">
          {message.annotations && Object.keys(message.annotations).length > 0 && (
            // Something happened to this turn that the text alone does not
            // show: a guardrail rewrote it, the user stopped it, or it was
            // never delivered. A user shown altered or truncated content
            // without being told cannot reason about what the model received.
            <span className="flex items-center gap-1 text-warning">
              <ShieldAlert className="h-3 w-3" />
              {Object.entries(message.annotations).map(annotationLabel).join(" · ")}
            </span>
          )}
          {!isUser && (message.model || message.output_tokens > 0) && (
            <span className="flex flex-wrap items-center gap-x-1.5 whitespace-nowrap text-muted-foreground/80">
              {message.model && <span>{message.model}</span>}
              {message.model && message.output_tokens > 0 && <span aria-hidden>·</span>}
              {message.output_tokens > 0 && (
                <span className="font-mono tabular-nums">
                  {message.input_tokens.toLocaleString()} in / {message.output_tokens.toLocaleString()} out
                </span>
              )}
            </span>
          )}
          {!isUser && canRemember && (
            <Button
              variant="ghost"
              size="sm"
              disabled={saving || remembered}
              onClick={() => void remember()}
              className="h-6 px-1.5 text-[11px] text-muted-foreground opacity-0 transition-opacity hover:text-foreground group-hover/message:opacity-100 focus-visible:opacity-100"
            >
              {remembered ? (
                <>
                  <Check className="mr-1 h-3 w-3" /> In memory
                </>
              ) : (
                <>
                  <BookmarkPlus className="mr-1 h-3 w-3" /> Save to memory
                </>
              )}
            </Button>
          )}
        </MessageFooter>
      </MessageContent>
    </Message>
  );
}

// The turn-outcome annotations the server writes read as sentences already;
// prefixing them with their key would only add noise.
const PLAIN_ANNOTATIONS = new Set(["stopped", "undelivered"]);

function annotationLabel([key, value]: [string, string]): string {
  if (key === "undelivered") return `Not delivered — ${value}`;
  if (PLAIN_ANNOTATIONS.has(key)) return value;
  return `${key}: ${value}`;
}

/** MessageAttachment renders one stored attachment referenced by a message. */
function MessageAttachment({ block }: { block: MessageBlock }) {
  const Icon = block.kind === "image" ? ImageIcon : block.kind === "pdf" ? FileText : Paperclip;
  return (
    <Attachment size="sm" className="min-w-0">
      <AttachmentMedia>
        <Icon />
      </AttachmentMedia>
      <AttachmentContent>
        <AttachmentTitle>{block.filename}</AttachmentTitle>
        <AttachmentDescription>{block.mime_type || block.kind}</AttachmentDescription>
      </AttachmentContent>
      <AttachmentTrigger asChild>
        <a
          href={api.attachmentDownloadUrl(block.attachment_id!)}
          target="_blank"
          rel="noreferrer"
          aria-label={`Download ${block.filename}`}
        />
      </AttachmentTrigger>
    </Attachment>
  );
}

/** PendingAttachmentChip renders a file selected but not yet sent. */
export function PendingAttachmentChip({
  filename,
  sizeBytes,
  uploading,
  onRemove,
}: {
  filename: string;
  sizeBytes: number;
  uploading: boolean;
  onRemove: () => void;
}) {
  return (
    <Attachment size="xs" state={uploading ? "uploading" : "done"}>
      <AttachmentMedia>
        <Paperclip />
      </AttachmentMedia>
      <AttachmentContent>
        <AttachmentTitle>{filename}</AttachmentTitle>
        <AttachmentDescription>
          {uploading ? "Uploading…" : formatBytes(sizeBytes)}
        </AttachmentDescription>
      </AttachmentContent>
      <AttachmentActions>
        <AttachmentAction
          onClick={onRemove}
          aria-label={`Remove ${filename}`}
          className="size-6"
        >
          <X className="h-3.5 w-3.5" />
        </AttachmentAction>
      </AttachmentActions>
    </Attachment>
  );
}
