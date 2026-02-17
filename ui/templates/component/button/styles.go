package button

func radiusFor(v Variant) string {
	if v == VariantPrimary {
		return "rounded-[var(--r-6)] "
	}
	return "rounded-2xl "
}

func focusFor(v Variant) string {
	if v == VariantDanger {
		return "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-danger/40 "
	}
	return "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40 "
}

func styleForButton(v Variant) string {
	switch v {
	case VariantMain:
		return "bg-primary text-white shadow-sm hover:shadow-md active:shadow-sm hover:bg-primary/90 "
	case VariantPrimary:
		return "bg-primary text-white hover:bg-primary/90 "
	case VariantSecondary:
		return "bg-card text-fg border border-border shadow-sm hover:border-primary/40 hover:shadow-md active:shadow-sm "
	case VariantGhost:
		return "bg-transparent text-fg hover:bg-nav-bg "
	case VariantDanger:
		return "bg-danger text-white hover:bg-danger/90 "
	default:
		return "bg-card text-fg border border-border hover:bg-nav-bg "
	}
}

func styleForLink(v Variant) string {
	switch v {
	case VariantMain:
		return "bg-card text-primary border border-border shadow-sm hover:border-primary/40 hover:shadow-md active:shadow-sm "
	case VariantPrimary:
		return "bg-primary text-white hover:bg-primary/90 "
	case VariantSecondary:
		return "bg-card text-fg border border-border hover:bg-nav-bg "
	case VariantGhost:
		return "bg-transparent text-fg hover:bg-nav-bg "
	case VariantDanger:
		return "bg-danger text-white hover:bg-danger/90 "
	default:
		return "bg-card text-fg border border-border hover:bg-nav-bg "
	}
}
