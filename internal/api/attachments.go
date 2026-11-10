package api

import (
	"net/http"

	"github.com/peasant-labs/peasant/internal/attachments"
)

func (s *Server) handleAttachment() http.Handler {
	return attachments.Handler(s.cfg.Store.AttachmentStore())
}
