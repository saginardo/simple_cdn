package control

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func (s *Server) listMessages(response http.ResponseWriter, request *http.Request) {
	if err := s.reconcileMessages(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	limit := 50
	if value := strings.TrimSpace(request.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(response, http.StatusBadRequest, errors.New("limit must be between 1 and 200"))
			return
		}
		limit = parsed
	}
	unreadOnly := request.URL.Query().Get("unread") == "1"
	page, err := s.Store.Messages(limit, unreadOnly)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, page)
}

func (s *Server) markMessageRead(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.MarkMessageRead(request.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, err)
			return
		}
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) markAllMessagesRead(response http.ResponseWriter, _ *http.Request) {
	if err := s.Store.MarkAllMessagesRead(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteMessage(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.DeleteMessage(request.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, err)
			return
		}
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) reconcileMessages() error {
	retentionCutoff := time.Now().UTC().AddDate(0, -3, 0)
	if err := s.Store.ReconcileTaskMessages(); err != nil {
		return err
	}
	if strings.TrimSpace(s.BackupStatusPath) != "" {
		status, err := ReadBackupRunStatus(s.BackupStatusPath)
		if err == nil && !status.UpdatedAt.Before(retentionCutoff) && (status.State == BackupRunRetrying || status.State == BackupRunSucceeded || status.State == BackupRunFailed || status.State == BackupRunSkipped) {
			severity := domain.MessageWarning
			title := "备份正在重试"
			if status.State == BackupRunSucceeded {
				severity, title = domain.MessageSuccess, "S3 备份成功"
			}
			if status.State == BackupRunFailed {
				severity, title = domain.MessageError, "S3 备份失败"
			}
			if status.State == BackupRunSkipped {
				severity, title = domain.MessageInfo, "S3 备份已跳过"
			}
			body := status.Error
			if body == "" {
				body = "备份任务已完成。"
				if status.State == BackupRunSkipped {
					body = "在线恢复切换期间未启动新的备份任务。"
				}
			}
			_, _, createErr := s.Store.CreateMessageOnce(domain.Message{
				Severity: severity, Category: "backup", Title: title, Body: body,
				SourceType: "backup", SourceID: status.StartedAt.Format("20060102T150405.000000000Z"), SourceStatus: status.State,
				CreatedAt: status.UpdatedAt,
			})
			if createErr != nil {
				return createErr
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(err.Error())))
			_, _, createErr := s.Store.CreateMessageOnce(domain.Message{
				Severity: domain.MessageError, Category: "backup", Title: "备份状态无法读取",
				Body:       "最近备份状态文件无效或不可读，请检查备份容器日志与文件权限。",
				SourceType: "backup_status", SourceID: "current", SourceStatus: "unreadable-" + digest[:16],
			})
			if createErr != nil {
				return createErr
			}
		}
	}
	if s.OnlineRestore != nil {
		job := s.OnlineRestore.Current()
		if job != nil && !job.VerifyOnly && !job.UpdatedAt.Before(retentionCutoff) && restoreStateNotifiable(job.State) {
			severity, title := restoreMessagePresentation(job.State)
			body := job.Detail
			if job.Error != "" {
				body = job.Error
			}
			_, _, err := s.Store.CreateMessageOnce(domain.Message{
				Severity: severity, Category: "restore", Title: title, Body: body,
				SourceType: "online_restore", SourceID: job.ID, SourceStatus: job.State,
				CreatedAt: job.UpdatedAt,
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func restoreStateNotifiable(state string) bool {
	return state == OnlineRestoreReady || state == OnlineRestoreCommitting || state == OnlineRestoreCompleted || state == OnlineRestoreFailed || state == OnlineRestoreCancelled
}

func restoreMessagePresentation(state string) (string, string) {
	switch state {
	case OnlineRestoreReady:
		return domain.MessageInfo, "S3 恢复已通过校验"
	case OnlineRestoreCommitting:
		return domain.MessageWarning, "S3 恢复正在切换"
	case OnlineRestoreCompleted:
		return domain.MessageSuccess, "S3 恢复成功"
	case OnlineRestoreCancelled:
		return domain.MessageInfo, "S3 恢复已取消"
	default:
		return domain.MessageError, "S3 恢复失败"
	}
}
