/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["../internal/webui/assets/**/*.{html,js}"],
  darkMode: ["selector", '[data-theme="dark"]'],
  theme: {
    extend: {
      colors: {
        brand: {
          50: "#eff6ff",
          100: "#dbeafe",
          200: "#bfdbfe",
          300: "#93c5fd",
          400: "#60a5fa",
          500: "#3b82f6",
          600: "#2563eb",
          700: "#1d4ed8",
          800: "#1e40af",
          900: "#1e3a8a",
          950: "#172554",
        },
      },
      fontFamily: {
        sans: ["Inter", "Segoe UI", "system-ui", "sans-serif"],
        mono: ["DM Mono", "SFMono-Regular", "monospace"],
      },
      boxShadow: {
        elevated: "0 24px 80px rgba(30, 58, 138, 0.12)",
        "elevated-dark": "0 24px 80px rgba(0, 0, 0, 0.4)",
      },
    },
  },
  plugins: [],
};
