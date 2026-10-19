import { useCallback, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  BookOpen,
  Cpu,
  FolderGit2,
  MessagesSquare,
  Plus,
  RotateCcw,
  ShieldAlert,
  Square,
  Trash2,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { useSession } from "@/hooks/use-session";
import { useTurn, type LiveTurn, type SentAttachment } from "@/hooks/use-turn";
import type { Conversation, Model } from "@/lib/types";
import { findModel, flattenModels, modelKey, modelLabel, parseModelKey } from "@/lib/models";
import { announceTurn } from "@/lib/announce";
import { Composer, emptyDraft, type Draft } from "@/components/composer";
import { MessageItem } from "@/components/message-item";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import {
  MessageScroller,
  MessageScrollerButton,
  MessageScrollerContent,
  MessageScrollerItem,
  MessageScrollerProvider,
  MessageScrollerViewport,
} from "@/components/ui/message-scroller";
import { Bubble, BubbleContent } from "@/components/ui/bubble";
import { Marker, MarkerContent, MarkerIcon } from "@/components/ui/marker";
import { Message, MessageContent } from "@/components/ui/message";
import { cn } from "@/lib/utils";

const NO_PROJECT = "__none__";

export function ChatPage() {
  const { conversationId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { me } = useSession();

  // Drafts belong to a conversation, not to the composer. React keeps this page
  // mounted across a `:conversationId` change, so a component-local draft would
  // follow the reader into the next thread and could be sent to it.
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [sidebarOpen, setSidebarOpen] = useState(false);

  const conversations = useQuery({
    queryKey: ["conversations"],
    queryFn: () => api.conversations(),
  });
  const models = useQuery({ queryKey: ["models"], queryFn: () => api.models() });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.projects() });

  const thread = useQuery({
    queryKey: ["conversation", conversationId],
    queryFn: () => api.conversation(conversationId!),
    enabled: !!conversationId,
  });

  const modelList: Model[] = useMemo(() => flattenModels(models.data), [models.data]);

  const conversation = thread.data?.conversation;
  // The model/provider pair is the identity: two accounts can advertise the
  // same model id, and matching on the id alone picks whichever is first.
  const selectedModel = findModel(modelList, conversation?.model, conversation?.provider_id);

  const reload = useCallback(
    async (id: string) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["conversation", id] }),
        queryClient.invalidateQueries({ queryKey: ["conversations"] }),
      ]);
    },
    [queryClient],
  );
  const { turnFor, isSending, send, cancel, dismiss } = useTurn(reload);
  const turn = turnFor(conversationId);
  const sending = isSending(conversationId);

  const draft = (conversationId && drafts[conversationId]) || emptyDraft;
  const updateDraft = useCallback(
    (update: (draft: Draft) => Draft) => {
      if (!conversationId) return;
      setDrafts((current) => ({
        ...current,
        [conversationId]: update(current[conversationId] ?? emptyDraft),
      }));
    },
    [conversationId],
  );

  const createConversation = useMutation({
    mutationFn: (input: Partial<Conversation>) => api.createConversation(input),
    onSuccess: (created) => {
      void queryClient.invalidateQueries({ queryKey: ["conversations"] });
      setSidebarOpen(false);
      navigate(`/c/${created.id}`);
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const updateConversation = useMutation({
    mutationFn: (input: Partial<Conversation>) => api.updateConversation(conversationId!, input),
    onSuccess: () => {
      if (conversationId) void reload(conversationId);
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const archive = useMutation({
    mutationFn: (id: string) => api.archiveConversation(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["conversations"] });
      navigate("/");
    },
    onError: (err: Error) => toast.error(err.message),
  });

  // MessageScroller owns follow-during-stream and "don't move the reader who
  // scrolled up" behavior, plus restore and jump-to-message. Scroll anchors
  // are the user turns, so a new reply follows the prompt that caused it.

  const onNewChat = () => {
    const firstModel = modelList[0];
    createConversation.mutate({
      model: firstModel?.id ?? "",
      provider_id: firstModel?.provider_id ?? "",
    });
  };

  const uploading = draft.attachments.some((a) => a.uploading);
  const canSubmit =
    !!conversationId &&
    !!conversation?.model &&
    !sending &&
    !uploading &&
    (draft.text.trim() !== "" || draft.attachments.length > 0);

  /**
   * submitDraft sends what the composer currently holds, and clears it only
   * once the server has taken the turn. A refusal before that point leaves the
   * message where the user left it, because nothing else holds a copy of it.
   */
  const submitDraft = useCallback(async () => {
    if (!conversationId || !canSubmit) return;
    const attachments: SentAttachment[] = draft.attachments
      .filter((a): a is typeof a & { id: string } => !!a.id)
      .map((a) => ({ id: a.id, filename: a.filename }));

    const accepted = await send(conversationId, {
      content: draft.text.trim(),
      attachments,
      model: conversation?.model,
      providerId: conversation?.provider_id,
    });
    if (accepted) setDrafts((current) => ({ ...current, [conversationId]: emptyDraft }));
  }, [canSubmit, conversationId, conversation?.model, conversation?.provider_id, draft, send]);

  const messages = thread.data?.messages ?? [];
  const list = conversations.data?.conversations ?? [];

  // The submitted prompt is shown from the live turn until the stored copy
  // arrives, so it is never invisible while the reply streams — and never
  // duplicated once the reload has it.
  const storedPrompt = turn?.start?.user_message_id;
  const promptIndex = storedPrompt ? messages.findIndex((m) => m.id === storedPrompt) : -1;
  const promptPending =
    !!turn &&
    turn.accepted &&
    promptIndex < 0 &&
    (turn.input.content !== "" || turn.input.attachments.length > 0);

  // A stopped turn's partial reply is stored by the server, so once the reload
  // brings it back the provisional copy would be a duplicate. The stored one
  // wins: it carries the annotations and the recorded cost.
  const storedReply =
    promptIndex >= 0 && messages.slice(promptIndex + 1).some((m) => m.role === "assistant");
  const showLiveTurn = !!turn && (turn.status === "streaming" || !storedReply);

  return (
    <div className="relative flex h-full">
      {/* Conversation list. A drawer on phones, a column from md up. */}
      {sidebarOpen && (
        <button
          aria-label="Close conversation list"
          onClick={() => setSidebarOpen(false)}
          className="absolute inset-0 z-30 bg-background/70 md:hidden"
        />
      )}
      <aside
        className={cn(
          "z-40 flex w-64 shrink-0 flex-col border-r border-border bg-background md:bg-card/30",
          "absolute inset-y-0 left-0 transition-transform md:static md:translate-x-0",
          sidebarOpen ? "translate-x-0 shadow-lg" : "-translate-x-full",
        )}
      >
        <div className="flex items-center gap-1 p-3 pb-2">
          <Button
            variant="outline"
            size="sm"
            className="h-9 min-w-0 flex-1 justify-start rounded-lg bg-transparent"
            onClick={onNewChat}
          >
            <Plus className="mr-1.5 h-3.5 w-3.5" />
            New chat
          </Button>
          <Button
            variant="ghost"
            size="sm"
            aria-label="Close conversation list"
            className="h-8 w-8 shrink-0 p-0 md:hidden"
            onClick={() => setSidebarOpen(false)}
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
        <nav aria-label="Conversations" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
          {conversations.isLoading ? (
            <div className="space-y-1.5 p-1">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
            </div>
          ) : conversations.isError ? (
            <div className="space-y-2 px-2 py-6 text-center">
              <p className="text-xs text-muted-foreground">Conversations could not be loaded.</p>
              <Button variant="outline" size="sm" onClick={() => void conversations.refetch()}>
                Try again
              </Button>
            </div>
          ) : list.length === 0 ? (
            <p className="px-2 py-6 text-center text-xs text-muted-foreground">
              No conversations yet.
            </p>
          ) : (
            <ul className="space-y-px">
              <li className="px-2.5 pb-1.5 pt-2 text-[11px] font-medium text-muted-foreground/70">
                Recent
              </li>
              {list.map((c) => (
                <li key={c.id} className="group/item relative">
                  <button
                    onClick={() => {
                      setSidebarOpen(false);
                      navigate(`/c/${c.id}`);
                    }}
                    className={cn(
                      "w-full truncate rounded-lg px-2.5 py-2 pr-7 text-left text-[13px] transition-colors",
                      c.id === conversationId
                        ? "bg-secondary text-foreground"
                        : "text-muted-foreground hover:bg-secondary/50 hover:text-foreground",
                    )}
                  >
                    {c.title || "Untitled"}
                  </button>
                  <button
                    onClick={() => archive.mutate(c.id)}
                    aria-label={`Archive ${c.title || "conversation"}`}
                    className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-1 text-muted-foreground opacity-0 transition-opacity hover:text-destructive group-hover/item:opacity-100 focus-visible:opacity-100"
                  >
                    <Trash2 className="h-3 w-3" />
                  </button>
                </li>
              ))}
            </ul>
          )}
        </nav>
      </aside>

      {/* Thread */}
      <section className="flex min-w-0 flex-1 flex-col">
        {!conversationId ? (
          <>
            <MobileBar onOpenSidebar={() => setSidebarOpen(true)} title="Conversations" />
            <EmptyState onNewChat={onNewChat} />
          </>
        ) : (
          <>
            <header className="flex h-12 shrink-0 items-center gap-2 border-b border-border px-2 sm:px-4">
              <Button
                variant="ghost"
                size="sm"
                aria-label="Open conversation list"
                className="h-8 w-8 shrink-0 p-0 md:hidden"
                onClick={() => setSidebarOpen(true)}
              >
                <MessagesSquare className="h-4 w-4" />
              </Button>
              <h1 className="min-w-0 flex-1 truncate font-sans text-sm font-medium">
                {conversation?.title || "New conversation"}
              </h1>
              {conversation?.project_id && (
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-8 shrink-0 gap-1.5 text-xs text-muted-foreground"
                  onClick={() => navigate(`/projects/${conversation.project_id}/memory`)}
                >
                  <BookOpen className="h-3.5 w-3.5" />
                  <span className="hidden sm:inline">Project memory</span>
                </Button>
              )}
            </header>

            <TurnAnnouncement turn={turn} threadLoading={thread.isLoading} />

            <MessageScrollerProvider autoScroll defaultScrollPosition="end">
              <MessageScroller className="min-h-0 flex-1">
                <MessageScrollerViewport>
                  <MessageScrollerContent className="mx-auto w-full max-w-3xl gap-8 px-4 pb-10 pt-8 sm:px-6">
                    {thread.isLoading ? (
                      <div className="space-y-6">
                        <Skeleton className="ml-auto h-10 w-2/3 rounded-2xl" />
                        <Skeleton className="h-28 w-full" />
                      </div>
                    ) : thread.isError ? (
                      <ThreadError
                        message={thread.error instanceof Error ? thread.error.message : ""}
                        onRetry={() => void thread.refetch()}
                      />
                    ) : (
                      messages.map((m) => (
                        <MessageScrollerItem
                          key={m.id}
                          messageId={m.id}
                          scrollAnchor={m.role === "user"}
                        >
                          <MessageItem
                            message={m}
                            conversationId={conversationId}
                            canRemember={!!conversation?.project_id}
                          />
                        </MessageScrollerItem>
                      ))
                    )}

                    {promptPending && turn && (
                      <MessageScrollerItem messageId="live-prompt" scrollAnchor>
                        <PendingPrompt input={turn.input} />
                      </MessageScrollerItem>
                    )}

                    {showLiveTurn && turn && (
                      <MessageScrollerItem messageId="live-turn">
                        <LiveTurnView
                          turn={turn}
                          // An accepted turn is resent as it was stored; a turn
                          // the server never took is still in the composer, so
                          // retrying it means sending what is there now.
                          onRetry={() =>
                            turn.accepted ? void send(conversationId, turn.input) : void submitDraft()
                          }
                          canRetry={turn.accepted || canSubmit}
                          onDismiss={() => dismiss(conversationId)}
                        />
                      </MessageScrollerItem>
                    )}
                  </MessageScrollerContent>
                </MessageScrollerViewport>
                <MessageScrollerButton />
              </MessageScroller>
            </MessageScrollerProvider>

            <div className="mx-auto w-full max-w-3xl shrink-0 px-2 pb-3 sm:px-6 sm:pb-5">
              <Composer
                disabled={!conversation?.model}
                sending={sending}
                canSend={canSubmit}
                model={selectedModel}
                attachmentsEnabled={!!me?.attachments.enabled}
                maxAttachmentBytes={me?.attachments.max_size_bytes ?? 0}
                conversationId={conversationId}
                draft={draft}
                onDraftChange={updateDraft}
                onSubmit={() => void submitDraft()}
                onCancel={() => cancel(conversationId)}
                controls={
                  <>
                    <Select
                      value={selectedModel ? modelKey(selectedModel) : ""}
                      onValueChange={(value) => {
                        const { id, provider_id } = parseModelKey(value);
                        updateConversation.mutate({ model: id, provider_id });
                      }}
                    >
                      <SelectTrigger
                        aria-label="Model"
                        className="h-8 w-auto min-w-0 max-w-[14rem] gap-1.5 rounded-lg border-transparent bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-secondary hover:text-foreground"
                      >
                        <Cpu className="h-3.5 w-3.5 shrink-0" />
                        <SelectValue placeholder="Select a model" />
                      </SelectTrigger>
                      <SelectContent>
                        {modelList.map((m) => (
                          <SelectItem key={modelKey(m)} value={modelKey(m)} className="text-xs">
                            {modelLabel(m, modelList)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>

                    <Select
                      value={conversation?.project_id || NO_PROJECT}
                      onValueChange={(value) =>
                        updateConversation.mutate({ project_id: value === NO_PROJECT ? "" : value })
                      }
                    >
                      <SelectTrigger
                        aria-label="Project"
                        className="h-8 w-auto min-w-0 max-w-[12rem] gap-1.5 rounded-lg border-transparent bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-secondary hover:text-foreground"
                      >
                        <FolderGit2 className="h-3.5 w-3.5 shrink-0" />
                        <SelectValue placeholder="No project" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={NO_PROJECT} className="text-xs">
                          No project
                        </SelectItem>
                        {(projects.data?.projects ?? []).map((p) => (
                          <SelectItem key={p.id} value={p.id} className="text-xs">
                            {p.display_name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </>
                }
              />
            </div>
          </>
        )}
      </section>
    </div>
  );
}

/**
 * TurnAnnouncement narrates the turn to a screen reader.
 *
 * Everything a sighted user learns from the transcript moving — the prompt
 * landing, the reply arriving, a refusal — is otherwise silent, because the
 * changes happen outside the focused element. Two regions, because a failure
 * should interrupt and progress should not.
 */
function TurnAnnouncement({
  turn,
  threadLoading,
}: {
  turn: LiveTurn | null;
  threadLoading: boolean;
}) {
  const { text, assertive } = announceTurn(turn, threadLoading);
  return (
    <>
      <div className="sr-only" role="status" aria-live="polite">
        {assertive ? "" : text}
      </div>
      <div className="sr-only" role="alert" aria-live="assertive">
        {assertive ? text : ""}
      </div>
    </>
  );
}

/** MobileBar gives the phone layout a way back to the conversation list. */
function MobileBar({ onOpenSidebar, title }: { onOpenSidebar: () => void; title: string }) {
  return (
    <div className="flex shrink-0 items-center gap-2 border-b border-border px-2 py-2 md:hidden">
      <Button
        variant="ghost"
        size="sm"
        aria-label="Open conversation list"
        className="h-8 w-8 shrink-0 p-0"
        onClick={onOpenSidebar}
      >
        <MessagesSquare className="h-4 w-4" />
      </Button>
      <span className="truncate text-sm text-muted-foreground">{title}</span>
    </div>
  );
}

/** PendingPrompt renders the submitted message before its stored copy loads. */
function PendingPrompt({ input }: { input: LiveTurn["input"] }) {
  return (
    <Message align="end">
      <MessageContent>
        <Bubble variant="secondary" align="end" className="max-w-[85%]">
          <BubbleContent className="rounded-2xl rounded-br-md px-4 py-2.5">
            {input.content ? (
              <Markdown content={input.content} />
            ) : (
              <span className="text-sm italic text-muted-foreground">(no text)</span>
            )}
            {input.attachments.length > 0 && (
              <p className="mt-2 text-xs text-muted-foreground">
                {input.attachments.map((a) => a.filename).join(", ")}
              </p>
            )}
          </BubbleContent>
        </Bubble>
      </MessageContent>
    </Message>
  );
}

function ThreadError({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="mx-auto max-w-sm space-y-3 py-10 text-center">
      <AlertTriangle className="mx-auto h-5 w-5 text-warning" />
      <p className="text-sm font-medium">This conversation could not be loaded.</p>
      {message && <p className="text-xs text-muted-foreground">{message}</p>}
      <Button variant="outline" size="sm" onClick={onRetry}>
        Try again
      </Button>
    </div>
  );
}

/** LiveTurnView renders the in-flight reply plus everything the server said about it. */
function LiveTurnView({
  turn,
  onRetry,
  canRetry,
  onDismiss,
}: {
  turn: LiveTurn;
  onRetry: () => void;
  canRetry: boolean;
  onDismiss: () => void;
}) {
  const failed = turn.status === "failed";
  const finished = failed || turn.status === "stopped" || turn.status === "done";
  return (
    <div className="space-y-3">
      {turn.start?.memory_degraded && (
        // Project memory failed to load. Saying so is essential: an answer given
        // without the project's memory looks identical to one given with it, and
        // silently degrading is how nobody notices memory has stopped working.
        <StatusMarker tone="warning" icon={AlertTriangle}>
          Project memory was unavailable for this turn
          {turn.start.memory_reason ? ` — ${turn.start.memory_reason}` : ""}. The answer below was
          written without it.
        </StatusMarker>
      )}
      {turn.start?.buffered && !turn.text && !turn.error && (
        <StatusMarker tone="muted" icon={ShieldAlert}>
          A review policy applies to this conversation, so the reply arrives all at once rather than
          streaming.
        </StatusMarker>
      )}
      {turn.notices.map((notice, i) => (
        <StatusMarker key={i} tone="muted" icon={AlertTriangle}>
          {notice}
        </StatusMarker>
      ))}

      {turn.text ? (
        <Message align="start">
          <MessageContent>
            <Bubble variant="ghost" className="w-full">
              <BubbleContent className="w-full overflow-visible">
                <Markdown content={turn.text} className="assistant-prose space-y-4" />
                {turn.reasoning && <ReasoningNote text={turn.reasoning} />}
              </BubbleContent>
            </Bubble>
          </MessageContent>
        </Message>
      ) : (
        turn.status === "streaming" &&
        !turn.error &&
        !turn.blocked && (
          <Marker className="py-1 text-xs">
            <MarkerIcon>
              <span className="block h-1.5 w-1.5 animate-pulse rounded-full bg-primary" />
            </MarkerIcon>
            <MarkerContent className="shimmer">Thinking…</MarkerContent>
          </Marker>
        )
      )}

      {turn.status === "stopped" && (
        <StatusMarker tone="muted" icon={Square}>
          You stopped this reply.
          {turn.text
            ? " What had arrived is kept in the conversation, marked as interrupted."
            : " Nothing had arrived yet, so only your message was kept."}
        </StatusMarker>
      )}
      {turn.blocked && (
        <StatusMarker tone="warning" icon={ShieldAlert}>
          {turn.blocked}
        </StatusMarker>
      )}
      {turn.error && (
        <StatusMarker tone="destructive" icon={AlertTriangle}>
          {turn.error}
        </StatusMarker>
      )}

      {failed && !turn.accepted && (
        <p className="text-xs text-muted-foreground">
          Nothing was sent — your message is still in the composer below. Retry sends it as it
          stands now.
        </p>
      )}

      {/* Retry never costs retyping: an accepted turn resends what was stored,
          and an unsent one resends the draft it left in the composer. */}
      {finished && (
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            disabled={!canRetry}
            onClick={onRetry}
          >
            <RotateCcw className="mr-1.5 h-3 w-3" />
            Retry
          </Button>
          <Button
            variant="ghost"
            size="sm"
            className="h-7 text-xs text-muted-foreground"
            onClick={onDismiss}
          >
            Dismiss
          </Button>
        </div>
      )}
    </div>
  );
}

/** ReasoningNote renders the thinking stream, collapsed: it is useful context
 *  but not part of the answer the user is reading. */
function ReasoningNote({ text }: { text: string }) {
  return (
    <details className="mt-2 rounded-md bg-muted/40 px-2 py-1.5 text-xs text-muted-foreground">
      <summary className="cursor-pointer select-none">Reasoning</summary>
      <pre className="mt-1.5 whitespace-pre-wrap font-sans">{text}</pre>
    </details>
  );
}

function StatusMarker({
  tone,
  icon: Icon,
  children,
}: {
  tone: "warning" | "destructive" | "muted";
  icon: typeof AlertTriangle;
  children: React.ReactNode;
}) {
  return (
    <Marker
      className={cn(
        "rounded-lg px-3 py-2 text-xs",
        tone === "warning" && "bg-warning/10 text-warning",
        tone === "destructive" && "bg-destructive/10 text-destructive",
        tone === "muted" && "bg-muted text-muted-foreground",
      )}
    >
      <MarkerIcon>
        <Icon className="h-3.5 w-3.5" />
      </MarkerIcon>
      <MarkerContent>{children}</MarkerContent>
    </Marker>
  );
}

function EmptyState({ onNewChat }: { onNewChat: () => void }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 px-6 pb-16 text-center">
      <span className="flex h-11 w-11 items-center justify-center rounded-xl bg-primary/10 ring-1 ring-inset ring-primary/25">
        <MessagesSquare className="h-5 w-5 text-primary" />
      </span>
      <div className="space-y-1.5">
        <h1 className="font-display text-xl font-semibold">Start a conversation</h1>
        <p className="max-w-sm text-sm text-muted-foreground">
          Pick a project to give the model your team's accumulated notes for that repository, or
          start without one.
        </p>
      </div>
      <Button onClick={onNewChat} className="rounded-lg">
        <Plus className="mr-1.5 h-4 w-4" />
        New chat
      </Button>
    </div>
  );
}
