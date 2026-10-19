import {
  createContext,
  useCallback,
  useContext,
  useState,
  type ReactNode,
} from "react";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";

export interface ConfirmOptions {
  title: string;
  description?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Renders the confirm button in the destructive style. */
  destructive?: boolean;
}

type ConfirmFn = (opts: ConfirmOptions) => Promise<boolean>;

const ConfirmContext = createContext<ConfirmFn | null>(null);

interface PendingConfirm {
  opts: ConfirmOptions;
  resolve: (ok: boolean) => void;
}

/**
 * Promise-based confirmation dialog. Mount <ConfirmProvider> once near the app
 * root, then `const confirm = useConfirm()` anywhere and `await confirm({...})`.
 * Resolves `true` on confirm, `false` on cancel/dismiss (overlay click / Esc).
 *
 * This is the single confirmation pattern for the whole console. Cancel is focused
 * first so a reflexive Enter dismisses rather than confirms a destructive action.
 */
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<PendingConfirm | null>(null);

  const confirm = useCallback<ConfirmFn>((opts) => {
    return new Promise<boolean>((resolve) => {
      setPending({ opts, resolve });
    });
  }, []);

  // Functional update so rapid open/close can't resolve a stale promise twice.
  const settle = useCallback((ok: boolean) => {
    setPending((current) => {
      current?.resolve(ok);
      return null;
    });
  }, []);

  const opts = pending?.opts;
  const title = opts?.title ?? "";
  const description = opts?.description;
  const confirmLabel = opts?.confirmLabel ?? "Confirm";
  const cancelLabel = opts?.cancelLabel ?? "Cancel";
  const destructive = opts?.destructive ?? false;

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <Dialog
        open={pending !== null}
        onOpenChange={(open) => {
          if (!open) settle(false);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline" onClick={() => settle(false)}>
                {cancelLabel}
              </Button>
            </DialogClose>
            <Button
              variant={destructive ? "destructive" : "default"}
              onClick={() => settle(true)}
            >
              {confirmLabel}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </ConfirmContext.Provider>
  );
}

export function useConfirm(): ConfirmFn {
  const confirm = useContext(ConfirmContext);
  if (!confirm) throw new Error("useConfirm must be used within ConfirmProvider");
  return confirm;
}
