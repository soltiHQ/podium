package visual

func badgeStyle(v Variant) string {
	switch v {

	case VariantPrimary:
		return "bg-primary/10 text-primary "

	case VariantSecondary:
		return "bg-card border border-border text-fg "

	case VariantSuccess:
		return "bg-success/10 text-success "

	case VariantDanger:
		return "bg-danger/10 text-danger "

	case VariantMuted:
		return "bg-fg/5 text-muted-strong "

	default:
		return "bg-card border border-border text-fg "
	}
}
