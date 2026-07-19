// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { remarkPrependBase } from './src/plugins/remark-prepend-base.mjs';

const BASE = '/core-agent';

// Phase 0 smoketest config. Only the 5 pages required to exercise the
// full layout surface are wired into the sidebar; the rest of the port
// happens in Phase 2 after look-and-feel is signed off.
//
// baseURL matches the production GH Pages path so relative links
// resolve identically in dev and in prod. If we cut over the deploy,
// the CI workflow will still override this via --base.
export default defineConfig({
  site: 'https://go-steer.github.io',
  base: BASE,
  markdown: {
    remarkPlugins: [remarkPrependBase(BASE)],
  },
  integrations: [
    starlight({
      title: 'core-agent',
      description:
        'A reusable Go-based agent built on the Google Agent Development Kit.',
      logo: undefined,
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/go-steer/core-agent',
        },
      ],
      editLink: {
        baseUrl:
          'https://github.com/go-steer/core-agent/edit/main/docs/site/',
      },
      // Inline script runs before Starlight's own ThemeProvider script,
      // pinning data-theme to 'light' before first paint. Belt-and-braces
      // with the theme.css overrides that already apply under both
      // [data-theme='light'] and [data-theme='dark'].
      head: [
        {
          tag: 'script',
          attrs: { 'is:inline': true },
          content: "document.documentElement.dataset.theme = 'light';",
        },
      ],
      // Palette + typography live in one file so the whole visual
      // system is swappable.
      customCss: ['./src/styles/theme.css'],
      // Empty component override drops the dark-mode toggle from the
      // navbar. Light-only site (see plan).
      components: {
        ThemeSelect: './src/components/ThemeSelect.astro',
        ThemeProvider: './src/components/ThemeProvider.astro',
        Hero: './src/components/Hero.astro',
      },
      // Full audience-first IA. Sub-sections use `autogenerate` so new
      // pages appear automatically once added under the source dir.
      // Two "Getting started" entries disambiguated by suffix.
      sidebar: [
        {
          label: 'Overview',
          items: [
            { label: 'Introduction', link: '/' },
            { label: 'Why core-agent', link: '/why-core-agent/' },
          ],
        },
        {
          label: 'Run the CLI',
          items: [
            { label: 'Getting started (CLI)', link: '/run/getting-started/' },
            { label: 'Interactive (TUI)', items: [{ autogenerate: { directory: 'run/interactive' } }] },
            { label: 'Autonomous (headless)', items: [{ autogenerate: { directory: 'run/autonomous' } }] },
            { label: 'Skills library', items: [{ autogenerate: { directory: 'run/skills-library' } }] },
          ],
        },
        {
          label: 'Embed the Library',
          items: [
            { label: 'Getting started (library)', link: '/embed/getting-started/' },
            { label: 'Guide', link: '/embed/guide/' },
            { label: 'API reference', link: '/embed/api/' },
          ],
        },
        {
          label: 'Concepts',
          items: [{ autogenerate: { directory: 'concepts' } }],
        },
        {
          label: 'Agent design',
          items: [{ autogenerate: { directory: 'agent-design' } }],
        },
        {
          label: 'Use cases',
          items: [{ autogenerate: { directory: 'use-cases' } }],
        },
        {
          label: 'Examples',
          link: '/examples/',
        },
        {
          label: 'Reference',
          items: [{ autogenerate: { directory: 'reference' } }],
        },
      ],
    }),
  ],
});
