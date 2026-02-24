package templates

import (
	"embed"
	"slices"
)

//go:embed base.js
var BaseJS string

//go:embed themes/*.css
var ThemeCSS embed.FS

//go:embed layouts/*.html
var LayoutHTML embed.FS

// AvailableTemplates lists all built-in templates
// Currently only "developer" is built-in. Users can provide custom templates via --template-path
var AvailableTemplates = []string{
	"developer",
}

// IsValidTemplate checks if a template name is valid
func IsValidTemplate(name string) bool {
	return slices.Contains(AvailableTemplates, name)
}
