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

// Multi-select dropdown styles.
const msWrapper = "relative "

const msTrigger = "flex flex-wrap items-center gap-1.5 " +
	"min-h-[2.75rem] w-full px-3 py-2 " +
	"rounded-[var(--r-xs)] " +
	"bg-card text-fg text-sm " +
	"border border-input " +
	"hover:border-border " +
	"focus:ring-2 focus:ring-primary/25 focus:border-primary " +
	"outline-none transition-colors cursor-pointer "

const msPlaceholder = "text-muted-strong text-sm "

const msTag = "inline-flex items-center gap-1 " +
	"rounded-full px-2.5 py-0.5 " +
	"text-[11px] font-medium " +
	"bg-primary/10 text-primary border border-primary/20 "

const msTagRemove = "hover:text-danger transition-colors cursor-pointer shrink-0 "

const msDropdown = "absolute z-10 bottom-full mb-1 w-full " +
	"max-h-48 overflow-y-auto " +
	"rounded-[var(--r-xs)] " +
	"bg-card border border-border shadow-2 " +
	"py-1 "

const msOption = "flex items-center gap-2 " +
	"px-3 py-2 text-sm text-fg " +
	"hover:bg-surface-dim " +
	"cursor-pointer transition-colors "

const msCheckbox = "w-4 h-4 rounded " +
	"border border-input " +
	"accent-primary pointer-events-none "

// Password modal styles.
const passwordError = "text-sm text-danger font-medium "
