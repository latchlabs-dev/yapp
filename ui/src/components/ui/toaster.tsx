/**
 * Re-export the Toaster from the shadcn/sonner-based implementation.
 *
 * Previously this was a Radix-UI-based custom implementation using
 * @/hooks/use-toast. As of the shadcn/ui migration (sonner), all
 * toast state and rendering is handled by the sonner library.
 *
 * Import `toast` from "sonner" directly in components:
 *   import { toast } from "sonner"
 *
 *   toast.success("Saved")
 *   toast.error("Failed", { description: err.message })
 */
export { Toaster } from "./sonner";
