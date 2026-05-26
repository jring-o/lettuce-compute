# Lettuce Platform

Web dashboard for the Lettuce distributed volunteer compute platform. Built with Next.js 15+, TypeScript, Tailwind CSS, and shadcn/ui.

## Setup

```bash
npm install
cp .env.local.example .env.local
# Edit .env.local with your database URL
npm run dev
```

The dev server starts on [http://localhost:3000](http://localhost:3000).

## Scripts

| Command | Description |
|---------|-------------|
| `npm run dev` | Start development server |
| `npm run build` | Production build |
| `npm run start` | Start production server |
| `npm run lint` | Run ESLint |
| `npm run format` | Format with Prettier |
| `npm test` | Run tests |

## Health Check

Visit `/health` to verify platform and database connectivity.
