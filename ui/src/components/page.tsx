import { cn } from "@/lib/utils";

interface PageContainerProps extends React.HTMLAttributes<HTMLDivElement> {
  children: React.ReactNode;
}

export function PageContainer({ className, children, ...props }: PageContainerProps) {
  return (
    <div
      data-slot="page-container"
      className={cn("space-y-6", className)}
      {...props}
    >
      {children}
    </div>
  );
}

interface PageHeadingProps {
  title: string;
  description?: string;
  children?: React.ReactNode;  // slot for action buttons
}

export function PageHeading({ title, description, children }: PageHeadingProps) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-3 pb-1">
      <div className="min-w-0 space-y-1">
        <h1 className="flex items-center gap-2 text-[22px] font-semibold leading-tight tracking-tight">
          {title}
        </h1>
        {description && (
          <p className="text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {children && <div className="flex items-center gap-2">{children}</div>}
    </div>
  );
}

interface DetailPageHeaderProps {
  title: string;
  backTo: string;
  backLabel?: string;
  children?: React.ReactNode;
}

import { ArrowLeft } from "lucide-react";
import { Link } from "react-router-dom";
import { Button } from "@/components/ui/button";

export function DetailPageHeader({
  title,
  backTo,
  backLabel = "Back",
  children,
}: DetailPageHeaderProps) {
  return (
    <div className="flex items-center gap-3">
      <Link
        to={backTo}
        className="text-muted-foreground hover:text-foreground transition-colors"
      >
        <Button variant="ghost" size="icon" className="h-8 w-8">
          <ArrowLeft className="h-4 w-4" />
          <span className="sr-only">{backLabel}</span>
        </Button>
      </Link>
      <h1 className="min-w-0 truncate text-[22px] font-semibold leading-tight tracking-tight">{title}</h1>
      {children && <div className="ml-auto flex items-center gap-2">{children}</div>}
    </div>
  );
}

interface EmptyStateProps {
  icon?: React.ComponentType<{ className?: string }>;
  title: string;
  description?: string;
  action?: React.ReactNode;
}

export function EmptyState({ icon: Icon, title, description, action }: EmptyStateProps) {
  return (
    <div className="flex flex-col items-center justify-center py-12 text-center">
      {Icon && (
        <span className="mb-3 flex h-10 w-10 items-center justify-center rounded-lg bg-secondary/60 ring-1 ring-inset ring-border">
          <Icon className="h-5 w-5 text-muted-foreground" />
        </span>
      )}
      <h3 className="text-base font-medium text-foreground">{title}</h3>
      {description && (
        <p className="mt-1 text-sm text-muted-foreground max-w-sm">{description}</p>
      )}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}
