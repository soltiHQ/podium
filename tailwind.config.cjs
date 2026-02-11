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
                danger: "rgb(var(--danger) / <alpha-value>)",
                success: "rgb(var(--success) / <alpha-value>)",

                "nav-bg": "rgb(var(--nav-link-bg) / <alpha-value>)",
                "nav-fg": "rgb(var(--nav-link-fg) / <alpha-value>)",
            },
            borderRadius: {
                6: "var(--r-6)",
                full: "var(--r-full)",
            },
        },
    },
    plugins: [],
};