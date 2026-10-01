import { Meta } from './meta';
import { Title } from './title';

export interface PageMetaProps {
  /** Page title - displayed in browser tab and social shares */
  title: string;
  /** Page description - used for SEO and social shares */
  description: string;
  /** Canonical URL path (e.g., '/docs/getting-started') */
  url?: string;
  /** Open Graph image URL for social sharing */
  image?: string;
  /** Page type - 'website' for general pages, 'article' for content pages */
  type?: 'website' | 'article';
  /** Site name for Open Graph */
  siteName?: string;
  /** Base URL for constructing full URLs (e.g., 'https://example.com') */
  baseUrl?: string;
}

/**
 * Unified component for page title, description, and social meta tags.
 *
 * Handles all SEO and social sharing meta tags in one place:
 * - `<title>` tag
 * - `description` meta tag
 * - Open Graph tags (og:title, og:description, og:image, og:url, og:type, og:site_name)
 * - Twitter Card tags (twitter:card, twitter:title, twitter:description, twitter:image)
 *
 * @example
 * ```tsx
 * <PageMeta
 *   title="Getting Started — My App"
 *   description="Learn how to get started with My App"
 *   url="/docs/getting-started"
 *   baseUrl="https://myapp.com"
 *   siteName="My App"
 * />
 * ```
 */
export function PageMeta({ title, description, url, image, type = 'website', siteName, baseUrl }: PageMetaProps) {
  const fullUrl = url && baseUrl ? `${baseUrl}${url.startsWith('/') ? url : `/${url}`}` : undefined;

  return (
    <>
      <Title>{title}</Title>
      <Meta name='description' content={description} />

      {/* Open Graph */}
      <Meta property='og:type' content={type} />
      {siteName && <Meta property='og:site_name' content={siteName} />}
      <Meta property='og:title' content={title} />
      <Meta property='og:description' content={description} />
      {fullUrl && <Meta property='og:url' content={fullUrl} />}
      {image && <Meta property='og:image' content={image} />}

      {/* Twitter Card */}
      <Meta name='twitter:card' content='summary_large_image' />
      <Meta name='twitter:title' content={title} />
      <Meta name='twitter:description' content={description} />
      {image && <Meta name='twitter:image' content={image} />}
    </>
  );
}
