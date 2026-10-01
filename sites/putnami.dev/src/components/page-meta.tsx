import { HeaderLink, PageMeta as BasePageMeta, Script, type PageMetaProps as BasePageMetaProps } from '@putnami/web';

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
 * Wraps @putnami/web PageMeta with site-specific configuration.
 * Adds canonical link and optional JSON-LD structured data.
 */
export function PageMeta({ image, jsonLd, ...props }: PageMetaProps) {
  const canonicalUrl = props.url ? `${BASE_URL}${props.url.startsWith('/') ? props.url : `/${props.url}`}` : undefined;

  return (
    <>
      <BasePageMeta {...props} siteName={SITE_NAME} baseUrl={BASE_URL} image={image || DEFAULT_IMAGE} />
      {canonicalUrl && <HeaderLink rel='canonical' href={canonicalUrl} />}
      {jsonLd && <Script type='application/ld+json'>{JSON.stringify(jsonLd)}</Script>}
    </>
  );
}
