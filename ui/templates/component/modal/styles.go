package modal

// Modal layout styles.
const overlay = "fixed inset-0 z-40 bg-black/40 backdrop-blur-[2px] "

const panel = "fixed inset-0 z-50 flex items-center justify-center p-4 "

const container = "w-full max-w-md " +
	"rounded-[var(--r-lg)] border border-border bg-card shadow-3 " +
	"overflow-visible "

const body = "p-6 space-y-2 "

const title = "text-base font-semibold text-fg "

const message = "text-sm text-muted-strong leading-relaxed "

const footer = "flex justify-end gap-2 px-6 py-4 border-t border-border bg-surface-dim " +
	"rounded-b-[var(--r-lg)] "

const formBody = "p-6 space-y-4 "

// Password modal styles.
const passwordError = "text-sm text-danger font-medium "
