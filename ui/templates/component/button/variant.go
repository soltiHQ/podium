package button

// Variant controls the visual style of a button.
type Variant string

const (
	VariantPrimary   Variant = "primary"
	VariantSecondary Variant = "secondary"
	VariantGhost     Variant = "ghost"
	VariantWarning   Variant = "warning"
	VariantDanger    Variant = "danger"
)
