// NB this code was based on https://github.com/hashicorp/packer/blob/370b67497e90785b71be1b6fcc6430de487d644e/provisioner/powershell/elevated.go

package update

import (
	_ "embed" // this is needed for using the go:embed directive
	"text/template"
)

// elevatedOptions holds the values injected into elevated-template.ps1 at provision time.
// The rendered script registers a Windows Scheduled Task under the given user, runs the
// given Command, and streams its output back to Packer until the task completes.
type elevatedOptions struct {
	Username        string
	Password        string
	TaskName        string
	TaskDescription string
	Command         string
}

//go:embed elevated-template.ps1
var elevatedTemplatePs1 string

// elevatedTemplate is the parsed Go text/template for elevated-template.ps1,
// rendered at provision time with an elevatedOptions value.
var elevatedTemplate = template.Must(
	template.New("Elevated").Parse(
		elevatedTemplatePs1))
