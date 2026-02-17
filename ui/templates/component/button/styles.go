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

func styleFor(v Variant, isLink bool) string {
	switch v {

	case VariantMain:
		if isLink {
			return "bg-card text-primary font-semibold border border-border shadow-sm hover:border-primary/40 hover:shadow-md active:shadow-sm "
		}
		return "bg-primary text-white font-semibold shadow-sm hover:shadow-md active:shadow-sm hover:bg-primary/90 "

	case VariantPrimary:
		return "bg-primary text-white font-semibold hover:bg-primary/90 "

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
