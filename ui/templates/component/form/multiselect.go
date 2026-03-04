package form

// Multi-select dropdown styles shared by modal forms and the spec builder.
const (
	MsWrapper = "relative "

	MsTrigger = "flex flex-wrap items-center gap-1.5 " +
		"min-h-[2.75rem] w-full px-3 py-2 " +
		"rounded-[var(--r-xs)] " +
		"bg-card text-fg text-sm " +
		"border border-input " +
		"hover:border-border " +
		"focus:ring-2 focus:ring-primary/25 focus:border-primary " +
		"outline-none transition-colors cursor-pointer "

	MsPlaceholder = "text-muted-strong text-sm "

	MsTag = "inline-flex items-center gap-1 " +
		"rounded-full px-2.5 py-0.5 " +
		"text-[11px] font-medium " +
		"bg-primary/10 text-primary border border-primary/20 "

	MsTagRemove = "hover:text-danger transition-colors cursor-pointer shrink-0 "

	MsDropdown = "absolute z-10 bottom-full mb-1 w-full " +
		"max-h-48 overflow-y-auto " +
		"rounded-[var(--r-xs)] " +
		"bg-card border border-border shadow-2 " +
		"py-1 "

	MsOption = "flex items-center gap-2 " +
		"px-3 py-2 text-sm text-fg " +
		"hover:bg-surface-dim " +
		"cursor-pointer transition-colors "

	MsCheckbox = "w-4 h-4 rounded " +
		"border border-input " +
		"accent-primary pointer-events-none "
)
