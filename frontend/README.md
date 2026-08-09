# simple_cdn UI

React 19, TypeScript, Vite, Tailwind CSS v4, and shadcn/ui source components for the control-plane console.

```bash
npm ci
npm run check
npm run dev
```

The Vite development server proxies `/api` to the local TLS control plane at `https://127.0.0.1:8443`. Production builds are written to `internal/control/web/dist` and embedded in the Go control binary.

Add shadcn components with the checked-in CLI configuration:

```bash
npx shadcn add <component>
```

Run the browser smoke and responsive layout checks with:

```bash
npx playwright install chromium
npm run test:e2e
```

The complete project documentation map is in [`docs/README.md`](../docs/README.md). Control-plane URLs and build requirements are documented in [`docs/CONFIGURATION.md`](../docs/CONFIGURATION.md).
