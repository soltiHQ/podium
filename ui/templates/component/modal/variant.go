package modal

type Variant string

const (
	VariantDefault Variant = "default"
	VariantDanger  Variant = "danger"
)

type Method string

const (
	MethodPost   Method = "post"
	MethodDelete Method = "delete"
	MethodPut    Method = "put"
	MethodPatch  Method = "patch"
)
