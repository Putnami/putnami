import { PageMeta as BasePageMeta, Script, type PageMetaProps as BasePageMetaProps, useLocale } from '@putnami/web';

export interface PageMetaProps extends Omit<BasePageMetaProps, 'siteName' | 'baseUrl' | 'image'> {
  image?: string;
  /** JSON-LD structured data object to inject into the page head */
  jsonLd?: Record<string, unknown>;
}

export const SITE_NAME = 'Putnami';
const DEFAULT_IMAGE = 'https://putnami.dev/assets/putnami-logo.png';
export const BASE_URL = 'https://putnami.dev';

/**
 * Page meta component with Putnami.dev defaults.
 * Wraps @putnami/web PageMeta with site-specific configuration and optional
 * JSON-LD structured data. The framework writes the canonical link and the
 * alternates of each locale; `url` is the page's path in the default locale.
 */
export function PageMeta({ image, jsonLd, url, ...props }: PageMetaProps) {
  const { localize } = useLocale();

  return (
    <>
      <BasePageMeta
        {...props}
        url={url ? localize(url) : undefined}
        siteName={SITE_NAME}
        baseUrl={BASE_URL}
        image={image || DEFAULT_IMAGE}
      />
      {jsonLd && <Script type='application/ld+json'>{JSON.stringify(jsonLd)}</Script>}
    </>
  );
}
