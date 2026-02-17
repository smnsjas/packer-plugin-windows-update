package update

import (
	"io"
	"strings"

	"github.com/hashicorp/packer-plugin-sdk/packer"
)

// UpdateUi wraps a packer.Ui to intercept completion signals from the Windows update
// PowerShell script. When the script outputs a line beginning with "Exiting with code ",
// the finished field is set to true, allowing the provisioner to detect that the script
// exited normally rather than being interrupted.
type UpdateUi struct {
	ui       packer.Ui
	finished bool
}

// NewUpdateUi returns an UpdateUi wrapping the provided Packer UI.
func NewUpdateUi(ui packer.Ui) *UpdateUi {
	return &UpdateUi{
		ui:       ui,
		finished: false,
	}
}

func (u *UpdateUi) Askf(s string, args ...any) (string, error) {
	return u.ui.Askf(s, args...)
}

func (u *UpdateUi) Ask(s string) (string, error) {
	return u.ui.Ask(s)
}

func (u *UpdateUi) Sayf(s string, args ...any) {
	u.ui.Sayf(s, args...)
}

// Say intercepts lines from the Windows update script. A line beginning with
// "Exiting with code " marks the script as finished and is suppressed from the
// underlying UI; all other lines are forwarded normally.
func (u *UpdateUi) Say(s string) {
	if strings.HasPrefix(s, "Exiting with code ") {
		u.finished = true
		return
	}
	u.ui.Say(s)
}

func (u *UpdateUi) Message(s string) {
	u.Say(s)
}

func (u *UpdateUi) Errorf(s string, args ...any) {
	u.ui.Errorf(s, args...)
}

func (u *UpdateUi) Error(s string) {
	u.ui.Error(s)
}

func (u *UpdateUi) Machine(t string, args ...string) {
	u.ui.Machine(t, args...)
}

func (u *UpdateUi) TrackProgress(src string, currentSize, totalSize int64, stream io.ReadCloser) (body io.ReadCloser) {
	return u.ui.TrackProgress(src, currentSize, totalSize, stream)
}
