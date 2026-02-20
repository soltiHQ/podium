module.exports = {
    darkMode: "class",
    content: [
        "./ui/templates/**/*.{templ,html}",
        "./ui/static/**/*.{js,ts}",
        "./ui/**/*.go",
    ],
    theme: {
        extend: {
            colors: {
                bg: "rgb(var(--bg) / <alpha-value>)",
                fg: "rgb(var(--fg) / <alpha-value>)",

                surface: "rgb(var(--surface) / <alpha-value>)",
                "surface-dim": "rgb(var(--surface-dim) / <alpha-value>)",
                "surface-container": "rgb(var(--surface-container) / <alpha-value>)",
                "surface-container-high": "rgb(var(--surface-container-high) / <alpha-value>)",

                card: "rgb(var(--card) / <alpha-value>)",
                "card-fg": "rgb(var(--card-fg) / <alpha-value>)",

                muted: "rgb(var(--muted) / <alpha-value>)",
                "muted-strong": "rgb(var(--muted-strong) / <alpha-value>)",

                icon: "rgb(var(--icon) / <alpha-value>)",
                "icon-active": "rgb(var(--icon-active) / <alpha-value>)",

                border: "rgb(var(--border) / <alpha-value>)",
                "border-2": "rgb(var(--border-2) / <alpha-value>)",
                input: "rgb(var(--input-border) / <alpha-value>)",

                primary: "rgb(var(--primary) / <alpha-value>)",
                "primary-bg": "rgb(var(--primary-bg) / <alpha-value>)",
                "on-primary": "rgb(var(--on-primary) / <alpha-value>)",
                "on-primary-container": "rgb(var(--on-primary-container) / <alpha-value>)",

                secondary: "rgb(var(--secondary) / <alpha-value>)",
                "secondary-bg": "rgb(var(--secondary-bg) / <alpha-value>)",

                danger: "rgb(var(--danger) / <alpha-value>)",
                "danger-bg": "rgb(var(--danger-bg) / <alpha-value>)",
                warning: "rgb(var(--warning) / <alpha-value>)",
                success: "rgb(var(--success) / <alpha-value>)",

                "nav-bg": "rgb(var(--nav-link-bg) / <alpha-value>)",
                "nav-fg": "rgb(var(--nav-link-fg) / <alpha-value>)",
            },
            borderRadius: {
                none: "var(--r-none)",
                xs: "var(--r-xs)",
                sm: "var(--r-sm)",
                md: "var(--r-md)",
                lg: "var(--r-lg)",
                xl: "var(--r-xl)",
                full: "var(--r-full)",
            },
            boxShadow: {
                1: "var(--shadow-1)",
                2: "var(--shadow-2)",
                3: "var(--shadow-3)",
            },
        },
    },
    plugins: [],
};
