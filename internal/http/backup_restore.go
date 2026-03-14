package http

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	maxBackupUploadSize  = 100 << 20 // 100 MB
	maxUnpackedSize      = 500 << 20 // 500 MB
	maxBackupFiles       = 10000
	backupExportBase     = "/tmp/goclaw-exports"
)

// backupManifest is written as manifest.json inside every archive.
type backupManifest struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`     // "agent" or "team"
	Mode     string `json:"mode"`     // "workspace" or "full"
	AgentID  string `json:"agent_id,omitempty"`
	AgentKey string `json:"agent_key,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

// BackupRestoreHandler handles backup and restore endpoints for agents and teams.
type BackupRestoreHandler struct {
	agents  store.AgentStore
	teams   store.TeamStore
	token   string
	dataDir string
	msgBus  *bus.MessageBus
	isOwner func(string) bool
}

// NewBackupRestoreHandler creates a new backup/restore handler.
func NewBackupRestoreHandler(agents store.AgentStore, teams store.TeamStore, token, dataDir string, msgBus *bus.MessageBus, isOwner func(string) bool) *BackupRestoreHandler {
	return &BackupRestoreHandler{
		agents:  agents,
		teams:   teams,
		token:   token,
		dataDir: dataDir,
		msgBus:  msgBus,
		isOwner: isOwner,
	}
}

func (h *BackupRestoreHandler) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token != "" {
			if extractBearerToken(r) != h.token {
				locale := extractLocale(r)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": i18n.T(locale, i18n.MsgUnauthorized)})
				return
			}
		}
		userID := extractUserID(r)
		ctx := store.WithLocale(r.Context(), extractLocale(r))
		if userID != "" {
			ctx = store.WithUserID(ctx, userID)
		}
		r = r.WithContext(ctx)
		next(w, r)
	}
}

const maxBackupsPerEntity = 5

// RegisterRoutes registers all backup/restore routes.
func (h *BackupRestoreHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/agents/{id}/backup", h.authMiddleware(h.handleAgentBackup))
	mux.HandleFunc("GET /v1/agents/{id}/backups", h.authMiddleware(h.handleAgentListBackups))
	mux.HandleFunc("GET /v1/agents/{id}/backups/{filename}", h.authMiddleware(h.handleAgentDownloadBackup))
	mux.HandleFunc("DELETE /v1/agents/{id}/backups/{filename}", h.authMiddleware(h.handleAgentDeleteBackup))
	mux.HandleFunc("POST /v1/agents/{id}/restore", h.authMiddleware(h.handleAgentRestore))
	mux.HandleFunc("POST /v1/teams/{id}/backup", h.authMiddleware(h.handleTeamBackup))
	mux.HandleFunc("GET /v1/teams/{id}/backups", h.authMiddleware(h.handleTeamListBackups))
	mux.HandleFunc("GET /v1/teams/{id}/backups/{filename}", h.authMiddleware(h.handleTeamDownloadBackup))
	mux.HandleFunc("DELETE /v1/teams/{id}/backups/{filename}", h.authMiddleware(h.handleTeamDeleteBackup))
	mux.HandleFunc("POST /v1/teams/{id}/restore", h.authMiddleware(h.handleTeamRestore))
}

// ---------- Agent Backup ----------

func (h *BackupRestoreHandler) handleAgentBackup(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	agentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "agent")})
		return
	}

	agent, err := h.agents.GetByID(r.Context(), agentID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgAgentNotFound, agentID.String())})
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "workspace"
	}
	if mode != "workspace" && mode != "full" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "mode must be 'workspace' or 'full'")})
		return
	}

	slug := agent.AgentKey
	if slug == "" {
		slug = agentID.String()[:8]
	}
	ts := time.Now().Format("20060102-150405")
	fileName := fmt.Sprintf("%s-%s-%s.tar.gz", slug, mode, ts)
	prefix := fmt.Sprintf("%s-%s-%s", slug, mode, ts)

	outDir := filepath.Join(backupExportBase, "agents", agentID.String())
	if err := os.MkdirAll(outDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to create backup directory")})
		return
	}

	outPath := filepath.Join(outDir, fileName)
	f, err := os.Create(outPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to create backup file")})
		return
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	// Write manifest
	manifest := backupManifest{
		Version:  1,
		Type:     "agent",
		Mode:     mode,
		AgentID:  agentID.String(),
		AgentKey: agent.AgentKey,
	}
	if err := writeTarJSON(tw, prefix+"/manifest.json", manifest); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to write manifest")})
		return
	}

	// If full mode: write agent config + context files
	if mode == "full" {
		agentConfig := map[string]any{
			"agent_key":            agent.AgentKey,
			"display_name":        agent.DisplayName,
			"frontmatter":         agent.Frontmatter,
			"provider":            agent.Provider,
			"model":               agent.Model,
			"context_window":      agent.ContextWindow,
			"max_tool_iterations": agent.MaxToolIterations,
			"restrict_to_workspace": agent.RestrictToWorkspace,
			"agent_type":          agent.AgentType,
			"tools_config":        agent.ToolsConfig,
			"sandbox_config":      agent.SandboxConfig,
			"subagents_config":    agent.SubagentsConfig,
			"memory_config":       agent.MemoryConfig,
			"compaction_config":   agent.CompactionConfig,
			"context_pruning":     agent.ContextPruning,
			"other_config":        agent.OtherConfig,
		}
		if err := writeTarJSON(tw, prefix+"/agent.json", agentConfig); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to write agent config")})
			return
		}

		ctxFiles, err := h.agents.GetAgentContextFiles(r.Context(), agentID)
		if err == nil {
			for _, cf := range ctxFiles {
				writeTarFile(tw, prefix+"/files/"+cf.FileName, []byte(cf.Content))
			}
		}
	}

	// Walk workspace directory
	wsFileCount := 0
	if agent.Workspace != "" {
		workspaceRoot := agent.Workspace
		slog.Info("agent backup: walking workspace", "workspace", workspaceRoot)
		if info, err := os.Stat(workspaceRoot); err == nil && info.IsDir() {
			filepath.Walk(workspaceRoot, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				// Skip symlinks
				if info.Mode()&os.ModeSymlink != 0 {
					return nil
				}
				rel, err := filepath.Rel(workspaceRoot, path)
				if err != nil || strings.Contains(rel, "..") {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return nil
				}
				writeTarFile(tw, prefix+"/workspace/"+rel, data)
				wsFileCount++
				return nil
			})
		} else {
			slog.Warn("agent backup: workspace dir not found or not a dir", "workspace", workspaceRoot, "error", err)
		}
	} else {
		slog.Warn("agent backup: agent.Workspace is empty", "agent_id", agentID)
	}
	slog.Info("agent backup: workspace files added", "count", wsFileCount)

	tw.Close()
	gw.Close()
	f.Close()

	fi, _ := os.Stat(outPath)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}

	emitAudit(h.msgBus, r, "agent.backup", "agent", agentID.String())
	slog.Info("agent backup created", "agent_id", agentID, "mode", mode, "file", fileName, "size", size)

	writeJSON(w, http.StatusOK, map[string]any{
		"filename":   fileName,
		"size":       size,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"mode":       mode,
	})
}

func (h *BackupRestoreHandler) handleAgentListBackups(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	agentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "agent")})
		return
	}

	dir := filepath.Join(backupExportBase, "agents", agentID.String())
	backups := listBackupFiles(dir)
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups, "max": maxBackupsPerEntity})
}

func (h *BackupRestoreHandler) handleAgentDeleteBackup(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	agentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "agent")})
		return
	}
	deleteBackupFile(w, r, filepath.Join(backupExportBase, "agents", agentID.String()), locale)
}

func (h *BackupRestoreHandler) handleAgentDownloadBackup(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	agentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "agent")})
		return
	}
	serveBackupFile(w, r, filepath.Join(backupExportBase, "agents", agentID.String()), locale)
}

func (h *BackupRestoreHandler) handleAgentRestore(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	agentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "agent")})
		return
	}

	agent, err := h.agents.GetByID(r.Context(), agentID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgAgentNotFound, agentID.String())})
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "workspace"
	}
	if scope != "workspace" && scope != "full" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "scope must be 'workspace' or 'full'")})
		return
	}

	// Get the tar.gz reader — either from upload or from existing backup file
	var tarReader io.Reader
	var cleanup func()

	filename := r.URL.Query().Get("filename")
	if filename != "" {
		// Validate filename — no path traversal
		if strings.Contains(filename, "/") || strings.Contains(filename, "..") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidFilename)})
			return
		}
		path := filepath.Join(backupExportBase, "agents", agentID.String(), filename)
		if !strings.HasPrefix(path, filepath.Join(backupExportBase, "agents", agentID.String())+string(filepath.Separator)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidPath)})
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgFileNotFound)})
			return
		}
		tarReader = f
		cleanup = func() { f.Close() }
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, maxBackupUploadSize)
		file, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgMissingFileField)})
			return
		}
		tarReader = file
		cleanup = func() { file.Close() }
	}
	defer cleanup()

	gr, err := gzip.NewReader(tarReader)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "invalid gzip archive")})
		return
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	var manifest backupManifest
	var agentConfig map[string]any
	contextFiles := map[string]string{}
	workspaceFiles := map[string][]byte{}
	var totalSize int64
	fileCount := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "corrupt archive")})
			return
		}

		// Security checks
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue
		}
		name := filepath.Clean(hdr.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			continue
		}

		totalSize += hdr.Size
		if totalSize > maxUnpackedSize {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgFileTooLarge)})
			return
		}
		fileCount++
		if fileCount > maxBackupFiles {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgFileTooLarge)})
			return
		}

		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil {
			continue
		}

		// Strip the prefix (first path component)
		parts := strings.SplitN(name, string(filepath.Separator), 2)
		if len(parts) < 2 {
			// Top-level file: check if it's manifest
			if parts[0] == "manifest.json" {
				json.Unmarshal(data, &manifest)
			}
			continue
		}
		rel := parts[1]

		switch {
		case rel == "manifest.json":
			json.Unmarshal(data, &manifest)
		case rel == "agent.json":
			json.Unmarshal(data, &agentConfig)
		case strings.HasPrefix(rel, "files/"):
			fileName := strings.TrimPrefix(rel, "files/")
			if fileName != "" && !strings.Contains(fileName, "/") {
				contextFiles[fileName] = string(data)
			}
		case strings.HasPrefix(rel, "workspace/"):
			wsRel := strings.TrimPrefix(rel, "workspace/")
			if wsRel != "" {
				workspaceFiles[wsRel] = data
			}
		}
	}

	// Validate manifest
	if manifest.Type != "agent" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "backup type must be 'agent'")})
		return
	}
	if manifest.AgentID != "" && manifest.AgentID != agentID.String() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "backup agent_id does not match")})
		return
	}

	restoredItems := []string{}

	// Restore workspace files
	if scope == "workspace" || scope == "full" {
		if agent.Workspace != "" && len(workspaceFiles) > 0 {
			// Clear existing workspace
			os.RemoveAll(agent.Workspace)
			os.MkdirAll(agent.Workspace, 0755)

			for rel, data := range workspaceFiles {
				// Security: prevent path traversal
				clean := filepath.Clean(rel)
				if strings.Contains(clean, "..") {
					continue
				}
				dest := filepath.Join(agent.Workspace, clean)
				if !strings.HasPrefix(dest, agent.Workspace+string(filepath.Separator)) {
					continue
				}
				os.MkdirAll(filepath.Dir(dest), 0755)
				os.WriteFile(dest, data, 0644)
			}
			restoredItems = append(restoredItems, "workspace")
		}
	}

	// Restore config + context files (full scope only)
	if scope == "full" {
		if agentConfig != nil {
			updates := map[string]any{}
			for _, key := range []string{
				"display_name", "frontmatter", "provider", "model",
				"context_window", "max_tool_iterations", "restrict_to_workspace",
				"agent_type", "tools_config", "sandbox_config", "subagents_config",
				"memory_config", "compaction_config", "context_pruning", "other_config",
			} {
				if v, ok := agentConfig[key]; ok {
					updates[key] = v
				}
			}
			if len(updates) > 0 {
				if err := h.agents.Update(r.Context(), agentID, updates); err != nil {
					slog.Warn("backup restore: failed to update agent config", "error", err)
				} else {
					restoredItems = append(restoredItems, "config")
				}
			}
		}

		if len(contextFiles) > 0 {
			for fileName, content := range contextFiles {
				if err := h.agents.SetAgentContextFile(r.Context(), agentID, fileName, content); err != nil {
					slog.Warn("backup restore: failed to set context file", "file", fileName, "error", err)
				}
			}
			restoredItems = append(restoredItems, "files")
		}
	}

	emitAudit(h.msgBus, r, "agent.restore", "agent", agentID.String())
	slog.Info("agent restored", "agent_id", agentID, "scope", scope, "restored", restoredItems)

	writeJSON(w, http.StatusOK, map[string]any{
		"restored": restoredItems,
		"scope":    scope,
	})
}

// ---------- Team Backup ----------

func (h *BackupRestoreHandler) handleTeamBackup(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "team")})
		return
	}

	team, err := h.teams.GetTeam(r.Context(), teamID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgNotFound, "team", teamID.String())})
		return
	}

	slug := team.Name
	if slug == "" {
		slug = teamID.String()[:8]
	}
	ts := time.Now().Format("20060102-150405")
	fileName := fmt.Sprintf("%s-workspace-%s.tar.gz", slug, ts)
	prefix := fmt.Sprintf("%s-workspace-%s", slug, ts)

	outDir := filepath.Join(backupExportBase, "teams", teamID.String())
	if err := os.MkdirAll(outDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to create backup directory")})
		return
	}

	outPath := filepath.Join(outDir, fileName)
	f, err := os.Create(outPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to create backup file")})
		return
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	// Write manifest
	manifest := backupManifest{
		Version:  1,
		Type:     "team",
		Mode:     "workspace",
		TeamID:   teamID.String(),
		TeamName: team.Name,
	}
	if err := writeTarJSON(tw, prefix+"/manifest.json", manifest); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to write manifest")})
		return
	}

	// List workspace files (all scopes)
	wsFiles, err := h.teams.ListWorkspaceFiles(r.Context(), teamID, "", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgFailedToList, "workspace files")})
		return
	}

	// Walk workspace files from disk
	teamDataDir := filepath.Join(h.dataDir, "teams", teamID.String())
	slog.Info("team backup: listing workspace files", "team_id", teamID, "dataDir", h.dataDir, "teamDataDir", teamDataDir, "file_count", len(wsFiles))
	wsFileCount := 0
	for _, wf := range wsFiles {
		// Build the expected disk path
		var diskPath string
		if wf.FilePath != "" {
			diskPath = wf.FilePath
		} else {
			diskPath = filepath.Join(teamDataDir, wf.ChatID, wf.FileName)
		}

		slog.Debug("team backup: reading file", "file_name", wf.FileName, "chat_id", wf.ChatID, "file_path", wf.FilePath, "disk_path", diskPath)
		data, err := os.ReadFile(diskPath)
		if err != nil {
			slog.Warn("team backup: failed to read file", "disk_path", diskPath, "error", err)
			continue
		}

		// Archive as workspace/<chatID>/<fileName>
		archivePath := filepath.Join("workspace", wf.ChatID, wf.FileName)
		writeTarFile(tw, prefix+"/"+archivePath, data)
		wsFileCount++
	}
	slog.Info("team backup: workspace files added to archive", "count", wsFileCount)

	tw.Close()
	gw.Close()
	f.Close()

	fi, _ := os.Stat(outPath)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}

	emitAudit(h.msgBus, r, "team.backup", "team", teamID.String())
	slog.Info("team backup created", "team_id", teamID, "file", fileName, "size", size)

	writeJSON(w, http.StatusOK, map[string]any{
		"filename":   fileName,
		"size":       size,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"mode":       "workspace",
	})
}

func (h *BackupRestoreHandler) handleTeamListBackups(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "team")})
		return
	}

	dir := filepath.Join(backupExportBase, "teams", teamID.String())
	backups := listBackupFiles(dir)
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups, "max": maxBackupsPerEntity})
}

func (h *BackupRestoreHandler) handleTeamDownloadBackup(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "team")})
		return
	}
	serveBackupFile(w, r, filepath.Join(backupExportBase, "teams", teamID.String()), locale)
}

func (h *BackupRestoreHandler) handleTeamDeleteBackup(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "team")})
		return
	}
	deleteBackupFile(w, r, filepath.Join(backupExportBase, "teams", teamID.String()), locale)
}

func (h *BackupRestoreHandler) handleTeamRestore(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "team")})
		return
	}

	team, err := h.teams.GetTeam(r.Context(), teamID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgNotFound, "team", teamID.String())})
		return
	}
	_ = team

	// Get the tar.gz reader
	var tarReader io.Reader
	var cleanupFn func()

	filename := r.URL.Query().Get("filename")
	if filename != "" {
		if strings.Contains(filename, "/") || strings.Contains(filename, "..") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidFilename)})
			return
		}
		path := filepath.Join(backupExportBase, "teams", teamID.String(), filename)
		if !strings.HasPrefix(path, filepath.Join(backupExportBase, "teams", teamID.String())+string(filepath.Separator)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidPath)})
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgFileNotFound)})
			return
		}
		tarReader = f
		cleanupFn = func() { f.Close() }
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, maxBackupUploadSize)
		file, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgMissingFileField)})
			return
		}
		tarReader = file
		cleanupFn = func() { file.Close() }
	}
	defer cleanupFn()

	gr, err := gzip.NewReader(tarReader)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "invalid gzip archive")})
		return
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	var manifest backupManifest
	// chatID -> fileName -> data
	workspaceFiles := map[string]map[string][]byte{}
	var totalSize int64
	fileCount := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "corrupt archive")})
			return
		}

		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue
		}
		name := filepath.Clean(hdr.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			continue
		}

		totalSize += hdr.Size
		if totalSize > maxUnpackedSize {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgFileTooLarge)})
			return
		}
		fileCount++
		if fileCount > maxBackupFiles {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgFileTooLarge)})
			return
		}

		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil {
			continue
		}

		// Strip prefix
		parts := strings.SplitN(name, string(filepath.Separator), 2)
		if len(parts) < 2 {
			if parts[0] == "manifest.json" {
				json.Unmarshal(data, &manifest)
			}
			continue
		}
		rel := parts[1]

		switch {
		case rel == "manifest.json":
			json.Unmarshal(data, &manifest)
		case strings.HasPrefix(rel, "workspace/"):
			wsRel := strings.TrimPrefix(rel, "workspace/")
			// wsRel is "chatID/fileName"
			wsParts := strings.SplitN(wsRel, string(filepath.Separator), 2)
			if len(wsParts) == 2 && wsParts[0] != "" && wsParts[1] != "" {
				chatID := wsParts[0]
				fName := wsParts[1]
				if workspaceFiles[chatID] == nil {
					workspaceFiles[chatID] = map[string][]byte{}
				}
				workspaceFiles[chatID][fName] = data
			}
		}
	}

	// Validate manifest
	if manifest.Type != "team" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "backup type must be 'team'")})
		return
	}
	if manifest.TeamID != "" && manifest.TeamID != teamID.String() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "backup team_id does not match")})
		return
	}

	// Restore workspace files
	teamDataDir := filepath.Join(h.dataDir, "teams", teamID.String())
	restoredCount := 0

	for chatID, files := range workspaceFiles {
		for fName, data := range files {
			// Security checks
			cleanName := filepath.Clean(fName)
			if strings.Contains(cleanName, "..") {
				continue
			}
			cleanChatID := filepath.Clean(chatID)
			if strings.Contains(cleanChatID, "..") {
				continue
			}

			destPath := filepath.Join(teamDataDir, cleanChatID, cleanName)
			if !strings.HasPrefix(destPath, teamDataDir+string(filepath.Separator)) {
				continue
			}

			os.MkdirAll(filepath.Dir(destPath), 0755)
			if err := os.WriteFile(destPath, data, 0644); err != nil {
				continue
			}

			// Upsert metadata
			wsFile := &store.TeamWorkspaceFileData{
				TeamID:   teamID,
				ChatID:   chatID,
				FileName: cleanName,
				FilePath: destPath,
				SizeBytes: int64(len(data)),
			}
			h.teams.UpsertWorkspaceFile(r.Context(), wsFile, nil)
			restoredCount++
		}
	}

	emitAudit(h.msgBus, r, "team.restore", "team", teamID.String())
	slog.Info("team restored", "team_id", teamID, "files_restored", restoredCount)

	writeJSON(w, http.StatusOK, map[string]any{
		"restored":       []string{"workspace"},
		"files_restored": restoredCount,
	})
}

// ---------- Helpers ----------

func serveBackupFile(w http.ResponseWriter, r *http.Request, dir, locale string) {
	filename := r.PathValue("filename")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidFilename)})
		return
	}
	path := filepath.Join(dir, filename)
	if !strings.HasPrefix(path, dir+string(filepath.Separator)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidPath)})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgFileNotFound)})
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if fi != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fi.Size()))
	}
	io.Copy(w, f)
}

func deleteBackupFile(w http.ResponseWriter, r *http.Request, dir, locale string) {
	filename := r.PathValue("filename")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidFilename)})
		return
	}
	path := filepath.Join(dir, filename)
	if !strings.HasPrefix(path, dir+string(filepath.Separator)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidPath)})
		return
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgFileNotFound)})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to delete backup")})
		return
	}
	slog.Info("backup deleted", "file", filename)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": filename})
}

func writeTarJSON(tw *tar.Writer, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeTarFile(tw, name, data)
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

type backupEntry struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at"`
}

func listBackupFiles(dir string) []backupEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []backupEntry{}
	}

	var result []backupEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mode := "workspace"
		if strings.Contains(e.Name(), "-full-") {
			mode = "full"
		}
		result = append(result, backupEntry{
			Filename:  e.Name(),
			Size:      info.Size(),
			Mode:      mode,
			CreatedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
	}

	// Sort newest first
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})

	return result
}
