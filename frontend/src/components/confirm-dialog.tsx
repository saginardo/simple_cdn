import { useEffect, useState } from "react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { t } from "@/lib/i18n";
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel = t("确认"),
  confirmation,
  busy = false,
  destructive = false,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  confirmLabel?: string;
  confirmation?: string;
  busy?: boolean;
  destructive?: boolean;
  onConfirm: () => void | Promise<void>;
}) {
  const [value, setValue] = useState("");
  useEffect(() => {
    if (!open) setValue("");
  }, [open]);
  const allowed = !confirmation || value === confirmation;
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        {confirmation ? (
          <div className="grid gap-2">
            <Label htmlFor="confirmation-input">
              {t("输入")}{" "}
              <span className="font-mono text-foreground">{confirmation}</span>{" "}
              {t("以确认")}
            </Label>
            <Input
              id="confirmation-input"
              value={value}
              onChange={(event) => setValue(event.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
          </div>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>{t("取消")}</AlertDialogCancel>
          <AlertDialogAction
            disabled={!allowed || busy}
            data-variant={destructive ? "destructive" : undefined}
            className={
              destructive
                ? "bg-destructive text-white hover:bg-destructive/90"
                : undefined
            }
            onClick={(event) => {
              event.preventDefault();
              void Promise.resolve(onConfirm()).catch(() => undefined);
            }}
          >
            {busy ? t("处理中...") : confirmLabel}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
