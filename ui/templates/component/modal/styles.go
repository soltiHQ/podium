package modal

const overlay = "fixed inset-0 z-40 bg-black/40 backdrop-blur-[2px] "

const panel = "fixed inset-0 z-50 flex items-center justify-center p-4 "

const container = "w-full max-w-md " +
	"rounded-xl border border-border bg-card shadow-lg " +
	"overflow-hidden "

const body = "p-6 space-y-2 "

const title = "text-base font-semibold text-fg "

const message = "text-sm text-muted-strong leading-relaxed "

const footer = "flex justify-end gap-2 px-6 py-4 border-t border-border bg-surface-dim "

func confirmButtonStyle(v Variant) string {
	base := "inline-flex items-center justify-center gap-2 " +
		"h-10 px-4 rounded-2xl " +
		"font-semibold text-white " +
		"transition-all duration-200 " +
		"focus-visible:outline-none focus-visible:ring-2 "

	switch v {
	case VariantDanger:
		return base + "bg-danger hover:bg-danger/90 focus-visible:ring-danger/40 "
	default:
		return base + "bg-primary hover:bg-primary/90 focus-visible:ring-primary/40 "
	}
}

const cancelButtonStyle = "inline-flex items-center justify-center gap-2 " +
	"h-10 px-4 rounded-2xl " +
	"bg-card text-fg border border-border shadow-sm " +
	"hover:border-primary/40 hover:shadow-md active:shadow-sm " +
	"transition-all duration-200 " +
	"focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40 "

const formBody = "p-6 space-y-4 "

const fieldLabel = "text-[11px] font-mono uppercase tracking-[0.1em] text-muted mb-1 "

const fieldDisabledInput = "block w-full h-11 px-4 " +
	"rounded-[var(--r-6)] " +
	"bg-card text-fg/40 " +
	"border border-border " +
	"cursor-not-allowed opacity-60 "

const editInputStyle = "block w-full h-11 px-4 " +
	"rounded-[var(--r-6)] " +
	"bg-card text-fg " +
	"placeholder:text-muted-strong " +
	"outline-none transition-colors " +
	"border border-input " +
	"hover:border-border " +
	"focus:ring-2 focus:ring-primary/25 focus:border-primary "
