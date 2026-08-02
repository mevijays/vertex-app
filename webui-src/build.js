const { execSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const root = __dirname;
const distDir = path.join(root, "dist");
const vendorDir = path.join(distDir, "vendor", "fontawesome");

fs.rmSync(distDir, { recursive: true, force: true });
fs.mkdirSync(path.join(vendorDir, "css"), { recursive: true });
fs.mkdirSync(path.join(vendorDir, "fonts"), { recursive: true });

execSync("npx tailwindcss -i ./input.css -o ./dist/styles.css --minify", {
  cwd: root,
  stdio: "inherit",
});

const faPkg = path.join(root, "node_modules", "font-awesome");
fs.copyFileSync(
  path.join(faPkg, "css", "font-awesome.min.css"),
  path.join(vendorDir, "css", "font-awesome.min.css"),
);
for (const file of fs.readdirSync(path.join(faPkg, "fonts"))) {
  fs.copyFileSync(path.join(faPkg, "fonts", file), path.join(vendorDir, "fonts", file));
}

console.log("webui build complete: dist/styles.css, dist/vendor/fontawesome/**");
