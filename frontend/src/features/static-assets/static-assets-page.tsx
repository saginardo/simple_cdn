import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CirclePlus,
  File,
  Files,
  Link2,
  LoaderCircle,
  Pencil,
  Search,
  Trash2,
  Upload,
} from "lucide-react";
import { useMemo, useState, type FormEvent, type ReactNode } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
import { CopyButton } from "@/components/copy-button";
import { ListPagination } from "@/components/list-pagination";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useListPagination } from "@/hooks/use-list-pagination";
import { api, errorMessage } from "@/lib/api";
import {
  formatBytes,
  formatDateTime,
  formatNumber,
  shortHash,
} from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  Site,
  StaticAsset,
  StaticAssetBinding,
  StaticAssetOverview,
} from "@/lib/types";

export function StaticAssetsPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [uploadOpen, setUploadOpen] = useState(false);
  const [renameID, setRenameID] = useState<string | null>(null);
  const [assignmentID, setAssignmentID] = useState<string | null>(null);
  const [deleteID, setDeleteID] = useState<string | null>(null);
  const query = useQuery({
    queryKey: ["static-assets"],
    queryFn: () => api<StaticAssetOverview>("/api/static-assets"),
  });
  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ["static-assets"] });
  const deleteMutation = useMutation({
    mutationFn: (assetID: string) =>
      api<{ ok: boolean }>(
        `/api/static-assets/${encodeURIComponent(assetID)}`,
        {
          method: "DELETE",
        },
      ),
    onSuccess: async () => {
      setDeleteID(null);
      setAssignmentID(null);
      await refresh();
      toast.success(t("静态资源已删除"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const assets = useMemo(() => {
    const normalized = search.trim().toLocaleLowerCase();
    if (!normalized) return query.data?.assets ?? [];
    return (query.data?.assets ?? []).filter((asset) =>
      [asset.name, asset.original_name, asset.sha256, asset.content_type].some(
        (value) => value.toLocaleLowerCase().includes(normalized),
      ),
    );
  }, [query.data?.assets, search]);
  const pagination = useListPagination(assets);
  const bindingCount =
    query.data?.assets.reduce(
      (total, asset) => total + asset.bindings.length,
      0,
    ) ?? 0;
  const totalBytes =
    query.data?.assets.reduce((total, asset) => total + asset.size_bytes, 0) ??
    0;
  const renameAsset = query.data?.assets.find((asset) => asset.id === renameID);
  const assignmentAsset = query.data?.assets.find(
    (asset) => asset.id === assignmentID,
  );
  const deleteAsset = query.data?.assets.find((asset) => asset.id === deleteID);

  return (
    <>
      <PageHeader
        title={t("资源")}
        description={t("内容寻址资源与站点精确路径分发")}
        actions={
          <Button onClick={() => setUploadOpen(true)}>
            <Upload />
            {t("上传资源")}
          </Button>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data ? (
          <>
            <div className="flex flex-wrap items-center gap-x-6 gap-y-2 border-y py-3 text-sm">
              <Stat
                icon={Files}
                label={t("资源")}
                value={formatNumber(query.data.assets.length)}
              />
              <Stat
                icon={File}
                label={t("存储量")}
                value={formatBytes(totalBytes)}
              />
              <Stat
                icon={Link2}
                label={t("分发路径")}
                value={formatNumber(bindingCount)}
              />
              <span className="ml-auto text-xs text-muted-foreground">
                {t("单文件上限 {value0}", {
                  value0: formatBytes(query.data.max_file_bytes),
                })}
              </span>
            </div>

            <section>
              <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
                <div>
                  <h2 className="text-base font-semibold">{t("资源库")}</h2>
                  <p className="text-xs text-muted-foreground">
                    {t("上传对象与已发布路径")}
                  </p>
                </div>
                <div className="relative w-full sm:w-72">
                  <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
                  <Input
                    aria-label={t("搜索静态资源")}
                    className="pl-8"
                    value={search}
                    onChange={(event) => {
                      setSearch(event.target.value);
                      pagination.setPage(1);
                    }}
                    placeholder={t("搜索名称、文件或 SHA-256")}
                  />
                </div>
              </div>
              <Panel>
                {assets.length ? (
                  <>
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>{t("资源")}</TableHead>
                          <TableHead>{t("类型 / 大小")}</TableHead>
                          <TableHead>SHA-256</TableHead>
                          <TableHead>{t("站点分发")}</TableHead>
                          <TableHead>{t("上传时间")}</TableHead>
                          <TableHead className="text-right">
                            {t("操作")}
                          </TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {pagination.items.map((asset) => (
                          <TableRow key={asset.id}>
                            <TableCell className="max-w-72">
                              <div
                                className="truncate font-medium"
                                title={asset.name}
                              >
                                {asset.name}
                              </div>
                              <div
                                className="truncate text-xs text-muted-foreground"
                                title={asset.original_name}
                              >
                                {asset.original_name}
                              </div>
                            </TableCell>
                            <TableCell>
                              <div className="text-xs">
                                {asset.content_type}
                              </div>
                              <div className="text-xs tabular-nums text-muted-foreground">
                                {formatBytes(asset.size_bytes)}
                              </div>
                            </TableCell>
                            <TableCell>
                              <div className="flex items-center gap-2">
                                <code className="text-xs" title={asset.sha256}>
                                  {shortHash(asset.sha256)}
                                </code>
                                <CopyButton
                                  value={asset.sha256}
                                  label={t("复制 SHA-256")}
                                />
                              </div>
                            </TableCell>
                            <TableCell>
                              <Button
                                variant="outline"
                                size="sm"
                                onClick={() => setAssignmentID(asset.id)}
                              >
                                <Link2 />
                                {asset.bindings.length
                                  ? t("{value0} 个路径", {
                                      value0: asset.bindings.length,
                                    })
                                  : t("分配站点")}
                              </Button>
                            </TableCell>
                            <TableCell className="whitespace-nowrap text-xs">
                              {formatDateTime(asset.created_at)}
                            </TableCell>
                            <TableCell>
                              <div className="flex justify-end gap-1">
                                <IconAction
                                  label={t("管理分发路径")}
                                  onClick={() => setAssignmentID(asset.id)}
                                >
                                  <Link2 />
                                </IconAction>
                                <IconAction
                                  label={t("重命名资源")}
                                  onClick={() => setRenameID(asset.id)}
                                >
                                  <Pencil />
                                </IconAction>
                                <IconAction
                                  label={t("删除资源")}
                                  onClick={() => setDeleteID(asset.id)}
                                >
                                  <Trash2 />
                                </IconAction>
                              </div>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                    <ListPagination
                      pagination={pagination}
                      itemLabel={t("个资源")}
                    />
                  </>
                ) : (
                  <div className="p-5">
                    <EmptyState
                      title={search ? t("未找到匹配资源") : t("暂无静态资源")}
                    />
                  </div>
                )}
              </Panel>
            </section>
          </>
        ) : null}
      </PageBody>

      <UploadAssetDialog
        key={String(uploadOpen)}
        open={uploadOpen}
        maxFileBytes={query.data?.max_file_bytes ?? 32 * 1024 * 1024}
        onOpenChange={setUploadOpen}
        onSaved={refresh}
      />
      <RenameAssetDialog
        key={renameAsset ? `rename-${renameAsset.id}` : "rename-closed"}
        asset={renameAsset ?? null}
        onOpenChange={(open) => {
          if (!open) setRenameID(null);
        }}
        onSaved={refresh}
      />
      <AssetAssignmentsDialog
        key={
          assignmentAsset
            ? `assignment-${assignmentAsset.id}`
            : "assignment-closed"
        }
        asset={assignmentAsset ?? null}
        sites={query.data?.sites ?? []}
        cachePresets={query.data?.cache_presets ?? []}
        onOpenChange={(open) => {
          if (!open) setAssignmentID(null);
        }}
        onSaved={refresh}
      />
      <ConfirmDialog
        open={Boolean(deleteAsset)}
        onOpenChange={(open) => {
          if (!open) setDeleteID(null);
        }}
        title={t("删除静态资源")}
        description={t("资源对象及其全部站点分发路径将被删除并重新发布。")}
        confirmation={deleteAsset?.name}
        confirmLabel={t("删除资源")}
        destructive
        busy={deleteMutation.isPending}
        onConfirm={async () => {
          if (deleteAsset) await deleteMutation.mutateAsync(deleteAsset.id);
        }}
      />
    </>
  );
}

function UploadAssetDialog({
  open,
  maxFileBytes,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  maxFileBytes: number;
  onOpenChange: (open: boolean) => void;
  onSaved: () => Promise<unknown>;
}) {
  const [name, setName] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const mutation = useMutation({
    mutationFn: () => {
      if (!file) throw new Error(t("请选择文件"));
      const body = new FormData();
      body.append("file", file);
      if (name.trim()) body.append("name", name.trim());
      return api<StaticAsset>("/api/static-assets", {
        method: "POST",
        body,
      });
    },
    onSuccess: async () => {
      await onSaved();
      onOpenChange(false);
      toast.success(t("静态资源已上传"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const tooLarge = Boolean(file && file.size > maxFileBytes);
  function submit(event: FormEvent) {
    event.preventDefault();
    if (file && !tooLarge) mutation.mutate();
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("上传静态资源")}</DialogTitle>
            <DialogDescription>
              {t("单文件上限 {value0}", { value0: formatBytes(maxFileBytes) })}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-5">
            <Field label={t("文件")} id="static-file">
              <Input
                id="static-file"
                type="file"
                required
                onChange={(event) => setFile(event.target.files?.[0] ?? null)}
              />
            </Field>
            {file ? (
              <div className="flex items-center justify-between gap-3 rounded-lg border px-3 py-2 text-xs">
                <span className="min-w-0 truncate">{file.name}</span>
                <span
                  className={
                    tooLarge ? "text-destructive" : "text-muted-foreground"
                  }
                >
                  {formatBytes(file.size)}
                </span>
              </div>
            ) : null}
            <Field label={t("显示名称（可选）")} id="static-name">
              <Input
                id="static-name"
                maxLength={120}
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder={file?.name}
              />
            </Field>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              {t("取消")}
            </Button>
            <Button
              type="submit"
              disabled={!file || tooLarge || mutation.isPending}
            >
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Upload />
              )}
              {t("上传")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function RenameAssetDialog({
  asset,
  onOpenChange,
  onSaved,
}: {
  asset: StaticAsset | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => Promise<unknown>;
}) {
  const [name, setName] = useState(asset?.name ?? "");
  const mutation = useMutation({
    mutationFn: () =>
      api<StaticAsset>(
        `/api/static-assets/${encodeURIComponent(asset?.id ?? "")}`,
        {
          method: "PUT",
          body: JSON.stringify({ name }),
        },
      ),
    onSuccess: async () => {
      await onSaved();
      onOpenChange(false);
      toast.success(t("资源名称已更新"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (asset && name.trim()) mutation.mutate();
  }
  return (
    <Dialog open={Boolean(asset)} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("重命名资源")}</DialogTitle>
            <DialogDescription>{asset?.original_name ?? ""}</DialogDescription>
          </DialogHeader>
          <div className="py-5">
            <Field label={t("显示名称")} id="rename-static-name">
              <Input
                id="rename-static-name"
                required
                maxLength={120}
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            </Field>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              {t("取消")}
            </Button>
            <Button type="submit" disabled={!name.trim() || mutation.isPending}>
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Pencil />
              )}
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function AssetAssignmentsDialog({
  asset,
  sites,
  cachePresets,
  onOpenChange,
  onSaved,
}: {
  asset: StaticAsset | null;
  sites: Site[];
  cachePresets: string[];
  onOpenChange: (open: boolean) => void;
  onSaved: () => Promise<unknown>;
}) {
  const [editing, setEditing] = useState<StaticAssetBinding | "new" | null>(
    null,
  );
  const [removeBinding, setRemoveBinding] = useState<StaticAssetBinding | null>(
    null,
  );
  const removeMutation = useMutation({
    mutationFn: (binding: StaticAssetBinding) =>
      api<{ ok: boolean }>(
        `/api/static-assets/${encodeURIComponent(binding.asset_id)}/bindings/${encodeURIComponent(binding.id)}`,
        { method: "DELETE" },
      ),
    onSuccess: async () => {
      setRemoveBinding(null);
      await onSaved();
      toast.success(t("分发路径已删除并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const bindings = (asset?.bindings ?? []).map((binding) => {
    const site = sites.find((candidate) => candidate.id === binding.site_id);
    return {
      binding,
      site,
      address: site?.domains[0]
        ? `https://${site.domains[0]}${binding.url_path}`
        : binding.url_path,
    };
  });
  return (
    <>
      <Dialog open={Boolean(asset)} onOpenChange={onOpenChange}>
        <DialogContent className="max-h-[calc(100vh-2rem)] min-w-0 overflow-y-auto sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>{t("站点分发")}</DialogTitle>
            <DialogDescription>{asset?.name ?? ""}</DialogDescription>
          </DialogHeader>
          {asset ? (
            <div className="grid min-w-0 gap-5">
              <div className="grid gap-1 border-y py-3 text-xs sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center">
                <div className="min-w-0">
                  <span className="text-muted-foreground">SHA-256 </span>
                  <code className="break-all">{asset.sha256}</code>
                </div>
                <span className="text-muted-foreground">
                  {asset.content_type} · {formatBytes(asset.size_bytes)}
                </span>
              </div>

              <div className="min-w-0">
                <div className="mb-2 flex items-center justify-between gap-3">
                  <Label>{t("已分发路径")}</Label>
                  <Button size="sm" onClick={() => setEditing("new")}>
                    <CirclePlus />
                    {t("新增路径")}
                  </Button>
                </div>
                {bindings.length ? (
                  <>
                    <div className="grid gap-2 sm:hidden">
                      {bindings.map(({ binding, site, address }) => (
                        <div
                          key={binding.id}
                          className="grid min-w-0 gap-3 rounded-lg border p-3"
                        >
                          <div className="flex min-w-0 items-start justify-between gap-2">
                            <div className="min-w-0">
                              <div className="font-medium">
                                {site?.name ?? binding.site_id}
                              </div>
                              <div className="text-xs text-muted-foreground">
                                {site?.domains.join(", ") || "--"}
                              </div>
                            </div>
                            <CopyButton
                              value={address}
                              label={t("复制访问地址")}
                            />
                          </div>
                          <code className="min-w-0 break-all text-xs">
                            {address}
                          </code>
                          <div className="flex items-center justify-between gap-2 border-t pt-2">
                            <span className="text-xs text-muted-foreground">
                              {cachePresetLabel(binding.cache_control)}
                            </span>
                            <div className="flex shrink-0 gap-1">
                              <IconAction
                                label={t("编辑分发路径")}
                                onClick={() => setEditing(binding)}
                              >
                                <Pencil />
                              </IconAction>
                              <IconAction
                                label={t("删除分发路径")}
                                onClick={() => setRemoveBinding(binding)}
                              >
                                <Trash2 />
                              </IconAction>
                            </div>
                          </div>
                        </div>
                      ))}
                    </div>
                    <div className="hidden min-w-0 overflow-hidden rounded-lg border sm:block">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>{t("站点")}</TableHead>
                            <TableHead>{t("访问地址")}</TableHead>
                            <TableHead>{t("缓存策略")}</TableHead>
                            <TableHead className="text-right">
                              {t("操作")}
                            </TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {bindings.map(({ binding, site, address }) => (
                            <TableRow key={binding.id}>
                              <TableCell>
                                <div className="font-medium">
                                  {site?.name ?? binding.site_id}
                                </div>
                                <div className="text-xs text-muted-foreground">
                                  {site?.domains.join(", ") || "--"}
                                </div>
                              </TableCell>
                              <TableCell className="max-w-96">
                                <div className="flex items-center gap-2">
                                  <code
                                    className="block truncate text-xs"
                                    title={address}
                                  >
                                    {address}
                                  </code>
                                  <CopyButton
                                    value={address}
                                    label={t("复制访问地址")}
                                  />
                                </div>
                              </TableCell>
                              <TableCell>
                                {cachePresetLabel(binding.cache_control)}
                              </TableCell>
                              <TableCell>
                                <div className="flex justify-end gap-1">
                                  <IconAction
                                    label={t("编辑分发路径")}
                                    onClick={() => setEditing(binding)}
                                  >
                                    <Pencil />
                                  </IconAction>
                                  <IconAction
                                    label={t("删除分发路径")}
                                    onClick={() => setRemoveBinding(binding)}
                                  >
                                    <Trash2 />
                                  </IconAction>
                                </div>
                              </TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    </div>
                  </>
                ) : (
                  <div className="rounded-lg border p-4">
                    <EmptyState title={t("尚未分发到站点")} />
                  </div>
                )}
              </div>

              {editing ? (
                <BindingEditor
                  key={editing === "new" ? "new" : editing.id}
                  asset={asset}
                  existing={editing === "new" ? null : editing}
                  sites={sites}
                  cachePresets={cachePresets}
                  onCancel={() => setEditing(null)}
                  onSaved={async () => {
                    setEditing(null);
                    await onSaved();
                  }}
                />
              ) : null}
            </div>
          ) : null}
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>{t("完成")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={Boolean(removeBinding)}
        onOpenChange={(open) => {
          if (!open) setRemoveBinding(null);
        }}
        title={t("删除分发路径")}
        description={t("该精确 URL 将从站点配置中移除并立即重新发布。")}
        confirmLabel={t("删除并发布")}
        destructive
        busy={removeMutation.isPending}
        onConfirm={async () => {
          if (removeBinding) await removeMutation.mutateAsync(removeBinding);
        }}
      />
    </>
  );
}

function BindingEditor({
  asset,
  existing,
  sites,
  cachePresets,
  onCancel,
  onSaved,
}: {
  asset: StaticAsset;
  existing: StaticAssetBinding | null;
  sites: Site[];
  cachePresets: string[];
  onCancel: () => void;
  onSaved: () => Promise<unknown>;
}) {
  const [siteID, setSiteID] = useState(existing?.site_id ?? sites[0]?.id ?? "");
  const [urlPath, setURLPath] = useState(
    existing?.url_path ?? defaultURLPath(asset.original_name),
  );
  const [cacheControl, setCacheControl] = useState(
    existing?.cache_control ?? cachePresets[0] ?? "public, max-age=3600",
  );
  const mutation = useMutation({
    mutationFn: () =>
      api<StaticAssetBinding>(
        existing
          ? `/api/static-assets/${encodeURIComponent(asset.id)}/bindings/${encodeURIComponent(existing.id)}`
          : `/api/static-assets/${encodeURIComponent(asset.id)}/bindings`,
        {
          method: existing ? "PUT" : "POST",
          body: JSON.stringify({
            site_id: siteID,
            url_path: urlPath,
            cache_control: cacheControl,
          }),
        },
      ),
    onSuccess: async () => {
      await onSaved();
      toast.success(t("分发路径已保存并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const validPath =
    urlPath.startsWith("/") &&
    urlPath !== "/" &&
    !/[\s?#]/.test(urlPath) &&
    !urlPath.includes("//") &&
    !urlPath.split("/").some((segment) => segment === "." || segment === "..");
  function submit(event: FormEvent) {
    event.preventDefault();
    if (siteID && validPath && cacheControl) mutation.mutate();
  }
  const selectedSite = sites.find((site) => site.id === siteID);
  return (
    <form className="grid gap-4 border-t pt-5" onSubmit={submit}>
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-sm font-semibold">
          {existing ? t("编辑分发路径") : t("新增分发路径")}
        </h3>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label={t("站点")} id="asset-site">
          <Select value={siteID} onValueChange={setSiteID}>
            <SelectTrigger id="asset-site" className="w-full">
              <SelectValue placeholder={t("选择站点")} />
            </SelectTrigger>
            <SelectContent>
              {sites.map((site) => (
                <SelectItem key={site.id} value={site.id}>
                  {site.name} · {site.domains[0] || "--"}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Field label={t("精确 URL 路径")} id="asset-url-path">
          <Input
            id="asset-url-path"
            required
            maxLength={1024}
            spellCheck={false}
            className="font-mono text-xs"
            value={urlPath}
            onChange={(event) => setURLPath(event.target.value)}
            placeholder="/assets/app.js"
          />
        </Field>
        <Field label={t("缓存策略")} id="asset-cache-control">
          <Select value={cacheControl} onValueChange={setCacheControl}>
            <SelectTrigger id="asset-cache-control" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {cachePresets.map((preset) => (
                <SelectItem key={preset} value={preset}>
                  {cachePresetLabel(preset)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <div className="grid gap-2">
          <Label>{t("访问地址")}</Label>
          <code className="flex h-8 min-w-0 items-center truncate rounded-lg border px-2.5 text-xs text-muted-foreground">
            {selectedSite?.domains[0]
              ? `https://${selectedSite.domains[0]}${urlPath}`
              : urlPath || "--"}
          </code>
        </div>
      </div>
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" onClick={onCancel}>
          {t("取消")}
        </Button>
        <Button
          type="submit"
          disabled={
            !siteID || !validPath || !cacheControl || mutation.isPending
          }
        >
          {mutation.isPending ? (
            <LoaderCircle className="animate-spin" />
          ) : (
            <Link2 />
          )}
          {t("保存并发布")}
        </Button>
      </div>
    </form>
  );
}

function IconAction({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label={label}
          onClick={onClick}
        >
          {children}
        </Button>
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  );
}

function Field({
  label,
  id,
  children,
}: {
  label: string;
  id: string;
  children: ReactNode;
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}

function Stat({
  icon: Icon,
  label,
  value,
}: {
  icon: typeof Files;
  label: string;
  value: string;
}) {
  return (
    <div className="flex items-center gap-2">
      <Icon className="size-4 text-muted-foreground" />
      <span className="text-muted-foreground">{label}</span>
      <span className="font-medium tabular-nums">{value}</span>
    </div>
  );
}

function cachePresetLabel(value: string) {
  return (
    (
      {
        "public, max-age=3600": t("缓存 1 小时"),
        "public, max-age=86400": t("缓存 1 天"),
        "public, max-age=31536000, immutable": t("永久缓存（不可变）"),
        "no-cache": t("每次重新验证"),
      } as Record<string, string>
    )[value] ?? value
  );
}

function defaultURLPath(filename: string) {
  const safe = filename
    .trim()
    .replace(/[^A-Za-z0-9._~-]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return `/${safe || "resource"}`;
}
