import tailwindcssAnimate from "tailwindcss-animate";

/** @type {import('tailwindcss').Config} */
export default {
  darkMode: ["class"],
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    container: {
      center: true,
      padding: "2rem",
      screens: {
        "2xl": "1400px",
      },
    },
    extend: {
      fontFamily: {
        sans: ["var(--font-sans)"],
        mono: ["var(--font-mono)"],
      },
      colors: {
        border: "hsl(var(--border))",
        input: "hsl(var(--input))",
        ring: "hsl(var(--ring))",
        background: "hsl(var(--background))",
        foreground: "hsl(var(--foreground))",
        primary: {
          DEFAULT: "hsl(var(--primary))",
          foreground: "hsl(var(--primary-foreground))",
        },
        secondary: {
          DEFAULT: "hsl(var(--secondary))",
          foreground: "hsl(var(--secondary-foreground))",
        },
        destructive: {
          DEFAULT: "hsl(var(--destructive))",
          foreground: "hsl(var(--destructive-foreground))",
        },
        muted: {
          DEFAULT: "hsl(var(--muted))",
          foreground: "hsl(var(--muted-foreground))",
        },
        accent: {
          DEFAULT: "hsl(var(--accent))",
          foreground: "hsl(var(--accent-foreground))",
        },
        popover: {
          DEFAULT: "hsl(var(--popover))",
          foreground: "hsl(var(--popover-foreground))",
        },
        card: {
          DEFAULT: "hsl(var(--card))",
          foreground: "hsl(var(--card-foreground))",
        },
        caesium: {
          cyan: "hsl(var(--caesium-cyan))",
          gold: "hsl(var(--caesium-gold))",
          void: "hsl(var(--caesium-void))",
        },
        // Brand surfaces / accents (extended palette).
        cyan: {
          DEFAULT: "hsl(var(--cyan))",
          glow: "hsl(var(--cyan-glow))",
          dim: "hsl(var(--cyan-dim))",
        },
        gold: {
          DEFAULT: "hsl(var(--gold))",
          dim: "hsl(var(--gold-dim))",
        },
        void: "hsl(var(--void))",
        midnight: "hsl(var(--midnight))",
        obsidian: "hsl(var(--obsidian))",
        graphite: "hsl(var(--graphite))",
        silt: "hsl(var(--silt))",
        // Text levels.
        "text-1": "hsl(var(--text-1))",
        "text-2": "hsl(var(--text-2))",
        "text-3": "hsl(var(--text-3))",
        "text-4": "hsl(var(--text-4))",
        // Status semantics.
        success: "hsl(var(--success))",
        warning: "hsl(var(--warning))",
        danger: "hsl(var(--danger))",
        running: "hsl(var(--running))",
        cached: "hsl(var(--cached))",
        // Chart palette (shadcn-compatible).
        "chart-1": "hsl(var(--chart-1))",
        "chart-2": "hsl(var(--chart-2))",
        "chart-3": "hsl(var(--chart-3))",
        "chart-4": "hsl(var(--chart-4))",
        "chart-5": "hsl(var(--chart-5))",
        sidebar: {
          DEFAULT: "hsl(var(--sidebar))",
          foreground: "hsl(var(--sidebar-foreground))",
          border: "hsl(var(--sidebar-border))",
          muted: "hsl(var(--sidebar-muted))",
          accent: "hsl(var(--sidebar-accent))",
        },
        dag: {
          bg: "hsl(var(--dag-bg))",
          grid: "hsl(var(--dag-grid))",
        },
        code: {
          bg: "hsl(var(--code-bg))",
          fg: "hsl(var(--code-fg))",
        },
      },
      borderRadius: {
        lg: "var(--radius)",
        md: "calc(var(--radius) - 2px)",
        sm: "calc(var(--radius) - 4px)",
      },
      keyframes: {
        "accordion-down": {
          from: { height: "0" },
          to: { height: "var(--radix-accordion-content-height)" },
        },
        "accordion-up": {
          from: { height: "var(--radix-accordion-content-height)" },
          to: { height: "0" },
        },
        "orbit-spin": {
          from: { transform: "rotate(0deg)" },
          to: { transform: "rotate(360deg)" },
        },
        "nucleus-pulse": {
          "0%, 100%": { transform: "scale(1)", opacity: "1" },
          "50%": { transform: "scale(1.18)", opacity: "0.85" },
        },
        "cyan-pulse": {
          "0%, 100%": {
            boxShadow:
              "0 0 0 0 hsl(var(--cyan) / 0.55), 0 0 14px 0 hsl(var(--cyan) / 0.5)",
          },
          "70%": {
            boxShadow:
              "0 0 0 9px hsl(var(--cyan) / 0), 0 0 14px 0 hsl(var(--cyan) / 0.5)",
          },
        },
        "gold-pulse": {
          "0%, 100%": {
            boxShadow:
              "0 0 0 0 hsl(var(--gold) / 0.55), 0 0 12px 0 hsl(var(--gold) / 0.4)",
          },
          "70%": {
            boxShadow:
              "0 0 0 8px hsl(var(--gold) / 0), 0 0 12px 0 hsl(var(--gold) / 0.4)",
          },
        },
      },
      animation: {
        "accordion-down": "accordion-down 0.2s ease-out",
        "accordion-up": "accordion-up 0.2s ease-out",
        "orbit-spin": "orbit-spin 22s linear infinite",
        "nucleus-pulse": "nucleus-pulse 2.4s ease-in-out infinite",
        "cyan-pulse": "cyan-pulse 1.6s ease-out infinite",
        "gold-pulse": "gold-pulse 2s ease-out infinite",
      },
    },
  },
  plugins: [tailwindcssAnimate],
}
