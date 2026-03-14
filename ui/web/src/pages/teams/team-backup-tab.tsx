import { useState, useEffect, useCallback, useRef } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Download, Upload, RotateCcw, Archive, Trash2 } from "lucide-react";
import { useHttp } from "@/hooks/use-ws";
import { toast } from "@/stores/use-toast-store";

interface TeamBackupTabProps {
  teamId: string;
}

interface BackupEntry {
  filename: string;
  size: number;
  mode: string;
  created_at: string;
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${(bytes / Math.pow(k, i)).toFixed(1)} ${sizes[i]}`;
}

export function TeamBackupTab({ teamId }: TeamBackupTabProps) {
  const { t } = useTranslation("teams");
  const http = useHttp();
  const [backups, setBackups] = useState<BackupEntry[]>([]);
  const [maxBackups, setMaxBackups] = useState(5);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [restoring, setRestoring] = useState(false);

  // Confirm restore dialog
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [confirmFilename, setConfirmFilename] = useState<string | null>(null);
  const [confirmFile, setConfirmFile] = useState<File | null>(null);

  // Confirm delete oldest dialog (when at max)
  const [limitOpen, setLimitOpen] = useState(false);

  // Confirm delete single backup dialog
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteFilename, setDeleteFilename] = useState<string | null>(null);

  const fileInputRef = useRef<HTMLInputElement>(null);

  const loadBackups = useCallback(async () => {
    try {
      const res = await http.get<{ backups: BackupEntry[]; max: number }>(`/v1/teams/${teamId}/backups`);
      setBackups(res.backups ?? []);
      if (res.max) setMaxBackups(res.max);
    } catch {
      // ignore
    } finally {
      setLoading(false);
    }
  }, [http, teamId]);

  useEffect(() => {
    loadBackups();
  }, [loadBackups]);

  const doCreateBackup = async () => {
    setCreating(true);
    try {
      await http.post(`/v1/teams/${teamId}/backup`);
      toast.success(t("backup.created"));
      await loadBackups();
    } catch {
      toast.error(t("backup.failed"));
    } finally {
      setCreating(false);
    }
  };

  const handleCreateBackup = () => {
    if (backups.length >= maxBackups) {
      setLimitOpen(true);
      return;
    }
    doCreateBackup();
  };

  const handleConfirmLimitBackup = async () => {
    setLimitOpen(false);
    const oldest = backups[backups.length - 1];
    if (oldest) {
      try {
        await http.delete(`/v1/teams/${teamId}/backups/${encodeURIComponent(oldest.filename)}`);
      } catch {
        toast.error(t("backup.deleteFailed"));
        return;
      }
    }
    await doCreateBackup();
  };

  const handleDeleteBackup = (filename: string) => {
    setDeleteFilename(filename);
    setDeleteOpen(true);
  };

  const handleConfirmDelete = async () => {
    if (!deleteFilename) return;
    try {
      await http.delete(`/v1/teams/${teamId}/backups/${encodeURIComponent(deleteFilename)}`);
      toast.success(t("backup.deleted"));
      await loadBackups();
    } catch {
      toast.error(t("backup.deleteFailed"));
    } finally {
      setDeleteOpen(false);
      setDeleteFilename(null);
    }
  };

  const handleDownload = async (filename: string) => {
    try {
      const blob = await http.downloadBlob(`/v1/teams/${teamId}/backups/${filename}`);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = filename;
      a.click();
      URL.revokeObjectURL(url);
    } catch {
      const baseUrl = window.location.origin;
      window.open(`${baseUrl}/v1/teams/${teamId}/backups/${filename}`, "_blank");
    }
  };

  const handleRestoreFromBackup = (filename: string) => {
    setConfirmFilename(filename);
    setConfirmFile(null);
    setConfirmOpen(true);
  };

  const handleRestoreFromFile = (file: File) => {
    setConfirmFile(file);
    setConfirmFilename(null);
    setConfirmOpen(true);
  };

  const handleConfirmRestore = async () => {
    setRestoring(true);
    try {
      if (confirmFilename) {
        await http.post(`/v1/teams/${teamId}/restore?filename=${encodeURIComponent(confirmFilename)}`);
      } else if (confirmFile) {
        const formData = new FormData();
        formData.append("file", confirmFile);
        await http.upload(`/v1/teams/${teamId}/restore`, formData);
      }
      toast.success(t("backup.restored"));
      setConfirmOpen(false);
    } catch {
      toast.error(t("backup.failed"));
    } finally {
      setRestoring(false);
    }
  };

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) {
      handleRestoreFromFile(file);
    }
    e.target.value = "";
  };

  return (
    <div className="space-y-6">
      <h3 className="text-lg font-semibold">{t("backup.title")}</h3>

      {/* Create Backup */}
      <div className="rounded-lg border p-4">
        <div className="flex items-center justify-between">
          <h4 className="text-sm font-medium text-muted-foreground">
            {t("backup.createBackup")}
          </h4>
          <div className="flex items-center gap-3">
            <span className="text-xs text-muted-foreground">
              {backups.length}/{maxBackups}
            </span>
            <Button onClick={handleCreateBackup} disabled={creating} size="sm">
              <Archive className="mr-1.5 h-3.5 w-3.5" />
              {creating ? t("backup.creating") : t("backup.createBackup")}
            </Button>
          </div>
        </div>
      </div>

      {/* Backup History */}
      <div className="rounded-lg border p-4">
        <h4 className="mb-3 text-sm font-medium text-muted-foreground">
          {t("backup.list")}
        </h4>
        {loading ? (
          <p className="text-sm text-muted-foreground">Loading...</p>
        ) : backups.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("backup.empty")}</p>
        ) : (
          <div className="space-y-3">
            {backups.map((b) => (
              <div
                key={b.filename}
                className="flex flex-col gap-2 rounded-md border p-3 sm:flex-row sm:items-center sm:justify-between"
              >
                <div className="min-w-0">
                  <p className="truncate font-mono text-sm">{b.filename}</p>
                  <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <Badge variant="secondary" className="text-[10px]">workspace</Badge>
                    <span>{formatBytes(b.size)}</span>
                    <span>{new Date(b.created_at).toLocaleString()}</span>
                  </div>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => handleDownload(b.filename)}
                  >
                    <Download className="mr-1 h-3.5 w-3.5" />
                    {t("backup.download")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => handleRestoreFromBackup(b.filename)}
                  >
                    <RotateCcw className="mr-1 h-3.5 w-3.5" />
                    {t("backup.restore")}
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="h-8 w-8 text-muted-foreground hover:text-destructive"
                    onClick={() => handleDeleteBackup(b.filename)}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Restore from File */}
      <div className="rounded-lg border p-4">
        <div className="flex items-center justify-between">
          <h4 className="text-sm font-medium text-muted-foreground">
            {t("backup.restoreFromFile")}
          </h4>
          <Button
            variant="outline"
            size="sm"
            onClick={() => fileInputRef.current?.click()}
          >
            <Upload className="mr-1.5 h-3.5 w-3.5" />
            {t("backup.selectFile")}
          </Button>
          <input
            ref={fileInputRef}
            type="file"
            accept=".tar.gz,.tgz"
            onChange={handleFileSelect}
            className="hidden"
          />
        </div>
      </div>

      {/* Confirm Restore Dialog */}
      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("backup.confirmTitle")}</DialogTitle>
            <DialogDescription>
              {t("backup.confirmDescription")}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmOpen(false)} disabled={restoring}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={handleConfirmRestore}
              disabled={restoring}
            >
              {restoring ? t("backup.restoring") : t("backup.confirmButton")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Confirm Delete Oldest (limit reached) */}
      <Dialog open={limitOpen} onOpenChange={setLimitOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("backup.limitTitle")}</DialogTitle>
            <DialogDescription>
              {t("backup.limitDescription", { max: maxBackups, oldest: backups[backups.length - 1]?.filename ?? "" })}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setLimitOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleConfirmLimitBackup} disabled={creating}>
              {creating ? t("backup.creating") : t("backup.limitConfirm")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Confirm Delete Single Backup */}
      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("backup.deleteTitle")}</DialogTitle>
            <DialogDescription>
              {t("backup.deleteDescription", { filename: deleteFilename ?? "" })}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleConfirmDelete}>
              {t("backup.deleteConfirm")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
