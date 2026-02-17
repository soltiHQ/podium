package datagrid

func containerClass(padY string) string {
	return "col-span-full flex flex-col items-center justify-center text-center " + padY
}

func messageClass(strong bool) string {
	if strong {
		return "mt-6 text-base font-medium text-muted-strong"
	}
	return "mt-6 text-base font-medium text-muted"
}
