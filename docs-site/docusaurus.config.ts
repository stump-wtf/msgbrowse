import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// ============================================================
// CONFIGURE THESE VALUES FOR YOUR PROJECT
// ============================================================
const PROJECT_TITLE = 'msgbrowse';
const PROJECT_TAGLINE = "A calm, private reading room for everything you've ever said.";
const GITHUB_URL = 'https://github.com/joestump/msgbrowse';
const SITE_URL = 'https://joestump.github.io';
const BASE_URL = '/msgbrowse/';
// ============================================================

const config: Config = {
  title: PROJECT_TITLE,
  tagline: PROJECT_TAGLINE,
  favicon: 'img/favicon.svg',

  future: {
    v4: true,
    // Docusaurus 3.10 made `faster` (rspack + SWC + Lightning CSS) the default
    // under `v4`, and it must now be an explicit @docusaurus/faster dependency.
    // Held off deliberately: it builds ~5x quicker, but its stricter HTML
    // minifier reports nested <a> elements in generated pages that the webpack
    // path never flagged. Adopting the bundler and fixing that markup is its
    // own change, tracked in #338 — not something to smuggle into a dependency
    // bump.
    faster: false,
  },

  url: SITE_URL,
  baseUrl: BASE_URL,

  organizationName: 'joestump',
  projectName: 'msgbrowse',

  onBrokenLinks: 'throw',

  markdown: {
    format: 'detect',
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs-generated',
          sidebarPath: './sidebars.ts',
          routeBasePath: '/',
          // Map each served page in docs-generated/ (gitignored, rebuilt on
          // every build) back to its true, committed source file.
          editUrl: ({docPath}) => {
            // Hand-authored product docs are copied verbatim from
            // docs-site/docs/ by scripts/copy-product-docs.js.
            if (docPath.startsWith('docs/')) {
              return `${GITHUB_URL}/edit/main/docs-site/${docPath}`;
            }
            // Generated landing/index/graph pages have no source file.
            if (/(^|\/)index\.mdx$/.test(docPath) || docPath === 'graph.mdx') {
              return undefined;
            }
            // ADRs: decisions/<name>.mdx <- docs/adr/<name>.md
            if (docPath.startsWith('decisions/')) {
              const source = docPath
                .replace(/^decisions\//, 'docs/adr/')
                .replace(/\.mdx$/, '.md');
              return `${GITHUB_URL}/edit/main/${source}`;
            }
            // Specs: specs/<domain>/<file>.mdx <- docs/openspec/specs/...
            if (docPath.startsWith('specs/')) {
              const source = docPath
                .replace(/^specs\//, 'docs/openspec/specs/')
                .replace(/\.mdx$/, '.md');
              return `${GITHUB_URL}/edit/main/${source}`;
            }
            return undefined;
          },
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: PROJECT_TITLE,
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
        },
        {
          type: 'docSidebar',
          sidebarId: 'decisionsSidebar',
          position: 'left',
          label: 'ADRs',
        },
        {
          type: 'docSidebar',
          sidebarId: 'specsSidebar',
          position: 'left',
          label: 'Specifications',
        },
        {
          href: GITHUB_URL,
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Documentation',
          items: [
            {
              label: 'Architecture Decisions',
              to: '/decisions',
            },
            {
              label: 'Specifications',
              to: '/specs',
            },
          ],
        },
        {
          title: 'Project',
          items: [
            {
              label: 'GitHub',
              href: GITHUB_URL,
            },
          ],
        },
      ],
      copyright: `Copyright ${new Date().getFullYear()}. Built with Docusaurus.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['go', 'bash'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
