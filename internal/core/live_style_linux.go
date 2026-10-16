package core

import (
	"io"
	"os"

	"github.com/vpsfreecz/confctl/internal/ui"
)

func (e *Engine) liveHeader(operation, detail string) ui.LiveHeader {
	header := ui.LiveHeader{Command: e.liveCommand, Operation: operation, Detail: detail}
	if e.Log != nil {
		header.LogPath = e.Log.Name()
	}
	return header
}

func (e *Engine) liveColor(out io.Writer) bool {
	if e.Color == "always" {
		return true
	}
	f, ok := out.(*os.File)
	return e.Color == "auto" && ok && f != nil && os.Getenv("NO_COLOR") == "" && sshIsTTY(f)
}

func (e *Engine) liveText(out io.Writer, text, style string) string {
	return ui.LiveText(text, style, e.liveColor(out))
}
