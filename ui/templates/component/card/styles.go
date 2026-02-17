package card

const linkExtras = `
	block
	transition-colors
	hover:border-primary/40
	cursor-pointer
	[&_*]:cursor-pointer
`

func linkClass(base string) string {
	return base + " " + linkExtras
}
