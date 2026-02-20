package form

// LabelClass is the Tailwind class for form field labels.
const LabelClass = "text-[11px] uppercase tracking-[0.05em] text-muted mb-1 "

const baseInput = "block w-full h-11 px-4 " +
	"rounded-[var(--r-xs)] " +
	"bg-card text-fg " +
	"placeholder:text-muted-strong " +
	"outline-none transition-colors " +
	"disabled:opacity-50 disabled:pointer-events-none "

const okBorder = "border border-input " +
	"hover:border-border " +
	"focus:ring-2 focus:ring-primary/25 focus:border-primary "

const errBorder = "border border-danger " +
	"hover:border-danger " +
	"focus:ring-2 focus:ring-danger/25 focus:border-danger "

// DisabledClass is the Tailwind class for read-only/disabled inputs.
const DisabledClass = "block w-full h-11 px-4 " +
	"rounded-[var(--r-xs)] " +
	"bg-card text-fg/40 " +
	"border border-border " +
	"cursor-not-allowed opacity-60 "

// BaseInputClass is the default input class (no error state).
const BaseInputClass = baseInput + okBorder

// InputClass returns the full Tailwind class string for an input.
func InputClass(hasErr bool) string {
	if hasErr {
		return baseInput + errBorder
	}
	return baseInput + okBorder
}

const selectArrow = "appearance-none pr-10 " +
	"bg-[length:16px_16px] bg-[right_12px_center] bg-no-repeat " +
	"bg-[url('data:image/svg+xml;charset=utf-8," +
	"%3Csvg%20xmlns%3D%22http%3A//www.w3.org/2000/svg%22%20width%3D%2216%22%20height%3D%2216%22%20" +
	"viewBox%3D%220%200%2024%2024%22%20fill%3D%22none%22%20stroke%3D%22%2371717a%22%20" +
	"stroke-width%3D%222%22%20stroke-linecap%3D%22round%22%20stroke-linejoin%3D%22round%22%3E" +
	"%3Cpolyline%20points%3D%226%209%2012%2015%2018%209%22/%3E%3C/svg%3E')] "

const selectMultiple = "appearance-none py-2 "

// SelectClass returns the full Tailwind class string for a select.
func SelectClass(hasErr bool, multiple bool) string {
	base := "block w-full px-4 " +
		"rounded-[var(--r-xs)] " +
		"bg-card text-fg " +
		"placeholder:text-muted-strong " +
		"outline-none transition-colors " +
		"disabled:opacity-50 disabled:pointer-events-none "

	if multiple {
		base += selectMultiple
	} else {
		base += "h-11 " + selectArrow
	}

	if hasErr {
		return base + errBorder
	}
	return base + okBorder
}
