import type { ReactNode } from "react";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";

/** Explicit confirmation protects commands which cannot be undone. */
export function ConfirmAction({ title, description, disabled, onConfirm, children }: { title: string; description: string; disabled?: boolean; onConfirm: () => void; children: ReactNode }) {
 return <AlertDialog><AlertDialogTrigger asChild><Button size="sm" variant="outline" disabled={disabled}>{children}</Button></AlertDialogTrigger><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>{title}</AlertDialogTitle><AlertDialogDescription>{description}</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>取消</AlertDialogCancel><AlertDialogAction onClick={onConfirm}>确认</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>;
}
