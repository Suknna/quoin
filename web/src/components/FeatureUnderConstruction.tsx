import type { ReactNode } from "react";
import { Construction } from "lucide-react";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";

/** Shared honest placeholder for modules whose domain capability is not available yet. */
export function FeatureUnderConstruction({ title = "能力建设中", description, children }: { title?: string; description?: ReactNode; children?: ReactNode }) {
  return <Empty><EmptyHeader><EmptyMedia variant="icon"><Construction /></EmptyMedia><EmptyTitle>{title}</EmptyTitle>{(description || children) && <EmptyDescription>{description ?? children}</EmptyDescription>}</EmptyHeader></Empty>;
}
