package button

func radiusFor(v Variant) string {
	if v == VariantPrimary {
		return "rounded-[var(--r-6)] "
	}
	return "rounded-2xl "
}

func focusFor(v Variant) string {
	switch v {
	case VariantDanger:
		return "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-danger/40 "
	case VariantWarning:
		return "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-warning/40 "
	default:
		return "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40 "
	}
}

func styleFor(v Variant) string {
	switch v {
	case VariantPrimary:
		return "bg-primary text-white font-semibold hover:bg-primary/90 "
	case VariantSecondary:
		return "bg-card text-fg border border-border shadow-sm hover:border-primary/40 hover:shadow-md active:shadow-sm "
	case VariantGhost:
		return "bg-transparent text-fg hover:bg-nav-bg "
	case VariantWarning:
		return "bg-warning text-white hover:bg-warning/90 "
	case VariantDanger:
		return "bg-danger text-white hover:bg-danger/90 "
	default:
		return "bg-card text-fg border border-border hover:bg-nav-bg "
	}
}
