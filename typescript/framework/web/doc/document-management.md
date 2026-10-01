# Document Management

Putnami React provides document helpers for managing HTML document metadata, SEO, and head elements. These helpers work on both server and client.

## Overview

Document helpers allow you to manage:
- Page titles
- Meta tags (description, Open Graph, Twitter Cards)
- Scripts and stylesheets
- Language and favicon
- Other head elements

## Document Helper

The `documentHelper()` function returns a helper instance that works in both SSR and browser contexts:

```typescript
import { documentHelper } from '@putnami/web';

const doc = documentHelper();
```

## Setting Page Title

### Using the Title Component

```tsx
import { Title } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Title>My Page Title</Title>
      <h1>Page Content</h1>
    </>
  );
}
```

### Using the Helper

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';

export default function Page() {
  useEffect(() => {
    const doc = documentHelper();
    doc.title = 'My Page Title';
  }, []);

  return <h1>Page Content</h1>;
}
```

### Dynamic Titles

```tsx
import { Title } from '@putnami/web';
import { useLoaderData } from '@putnami/web';

export default function PostPage() {
  const { post } = useLoaderData<{ post: Post }>();

  return (
    <>
      <Title>{post.title} - My Blog</Title>
      <article>{post.content}</article>
    </>
  );
}
```

## Meta Tags

### Basic Meta Tags

```tsx
import { Meta } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Meta name="description" content="Page description" />
      <Meta name="keywords" content="keyword1, keyword2" />
      <Meta name="author" content="Author Name" />
    </>
  );
}
```

### Open Graph Tags

```tsx
import { Meta } from '@putnami/web';

export default function ArticlePage() {
  const { article } = useLoaderData<{ article: Article }>();

  return (
    <>
      <Meta property="og:title" content={article.title} />
      <Meta property="og:description" content={article.excerpt} />
      <Meta property="og:image" content={article.imageUrl} />
      <Meta property="og:type" content="article" />
      <Meta property="og:url" content={`https://example.com/articles/${article.slug}`} />
    </>
  );
}
```

### Twitter Card Tags

```tsx
import { Meta } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Meta name="twitter:card" content="summary_large_image" />
      <Meta name="twitter:title" content="Page Title" />
      <Meta name="twitter:description" content="Page description" />
      <Meta name="twitter:image" content="https://example.com/image.jpg" />
    </>
  );
}
```

### Using the Helper

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';

export default function Page() {
  useEffect(() => {
    const doc = documentHelper();
    doc.addMeta({ name: 'description', content: 'Page description' });
    doc.addMeta({ property: 'og:title', content: 'Page Title' });
  }, []);

  return <h1>Content</h1>;
}
```

## Scripts

### Adding Scripts

```tsx
import { Script } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Script src="https://example.com/analytics.js" />
      <Script src="/custom.js" async />
      <Script>
        {`
          console.log('Inline script');
        `}
      </Script>
    </>
  );
}
```

### Using the Helper

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';

export default function Page() {
  useEffect(() => {
    const doc = documentHelper();
    doc.addScript({
      src: 'https://example.com/analytics.js',
      async: true,
    });
  }, []);

  return <h1>Content</h1>;
}
```

### Conditional Scripts

```tsx
import { Script } from '@putnami/web';
import { useLoaderData } from '@putnami/web';

export default function Page() {
  const { needsAnalytics } = useLoaderData<{ needsAnalytics: boolean }>();

  return (
    <>
      {needsAnalytics && (
        <Script src="https://example.com/analytics.js" />
      )}
    </>
  );
}
```

## Stylesheets

### Adding Styles

```tsx
import { Style } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Style href="/styles.css" />
      <Style>
        {`
          body { background: #fff; }
        `}
      </Style>
    </>
  );
}
```

### Using the Helper

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';

export default function Page() {
  useEffect(() => {
    const doc = documentHelper();
    doc.addStyle({ href: '/custom.css' });
  }, []);

  return <h1>Content</h1>;
}
```

## Links

### Adding Links

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';

export default function Page() {
  useEffect(() => {
    const doc = documentHelper();
    doc.addLink({ rel: 'stylesheet', href: '/styles.css' });
    doc.addLink({ rel: 'preconnect', href: 'https://fonts.googleapis.com' });
    doc.addLink({ rel: 'icon', href: '/favicon.ico' });
  }, []);

  return <h1>Content</h1>;
}
```

## Language

### Setting Language

```tsx
import { Lang } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Lang>fr</Lang>
      <h1>Contenu</h1>
    </>
  );
}
```

### Using the Helper

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';

export default function Page() {
  useEffect(() => {
    const doc = documentHelper();
    doc.lang = 'fr';
  }, []);

  return <h1>Content</h1>;
}
```

## Favicon

### Setting Favicon

```tsx
import { Favicon } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Favicon href="/favicon.ico" />
      <h1>Content</h1>
    </>
  );
}
```

## SEO Best Practices

### 1. Unique Titles for Each Page

```tsx
import { Title } from '@putnami/web';
import { useLoaderData } from '@putnami/web';

export default function PostPage() {
  const { post } = useLoaderData<{ post: Post }>();

  return (
    <>
      <Title>{post.title} - My Blog</Title>
      <article>{post.content}</article>
    </>
  );
}
```

### 2. Descriptive Meta Descriptions

```tsx
import { Meta } from '@putnami/web';

export default function Page() {
  return (
    <Meta
      name="description"
      content="A comprehensive guide to building React applications with Putnami"
    />
  );
}
```

### 3. Open Graph for Social Sharing

```tsx
import { Meta } from '@putnami/web';
import { useLoaderData } from '@putnami/web';

export default function ArticlePage() {
  const { article } = useLoaderData<{ article: Article }>();
  const baseUrl = 'https://example.com';

  return (
    <>
      <Meta property="og:title" content={article.title} />
      <Meta property="og:description" content={article.excerpt} />
      <Meta property="og:image" content={`${baseUrl}${article.imageUrl}`} />
      <Meta property="og:url" content={`${baseUrl}/articles/${article.slug}`} />
      <Meta property="og:type" content="article" />
      <Meta property="article:published_time" content={article.publishedAt} />
      <Meta property="article:author" content={article.author} />
    </>
  );
}
```

### 4. Canonical URLs

```tsx
import { documentHelper } from '@putnami/web';
import { useEffect } from 'react';
import { useLocation } from '@putnami/web';

export default function Page() {
  const location = useLocation();
  const baseUrl = 'https://example.com';

  useEffect(() => {
    const doc = documentHelper();
    doc.addLink({
      rel: 'canonical',
      href: `${baseUrl}${location.pathname}`,
    });
  }, [location.pathname]);

  return <h1>Content</h1>;
}
```

### 5. Structured Data (JSON-LD)

```tsx
import { Script } from '@putnami/web';
import { useLoaderData } from '@putnami/web';

export default function ArticlePage() {
  const { article } = useLoaderData<{ article: Article }>();

  const structuredData = {
    '@context': 'https://schema.org',
    '@type': 'Article',
    headline: article.title,
    description: article.excerpt,
    image: article.imageUrl,
    datePublished: article.publishedAt,
    author: {
      '@type': 'Person',
      name: article.author,
    },
  };

  return (
    <>
      <Script type="application/ld+json">
        {JSON.stringify(structuredData)}
      </Script>
      <article>{article.content}</article>
    </>
  );
}
```

## Complete SEO Example

```tsx
import { Title, Meta, Script } from '@putnami/web';
import { useLoaderData } from '@putnami/web';
import { useLocation } from '@putnami/web';

export default function BlogPostPage() {
  const { post } = useLoaderData<{ post: Post }>();
  const location = useLocation();
  const baseUrl = 'https://example.com';
  const canonicalUrl = `${baseUrl}${location.pathname}`;

  const structuredData = {
    '@context': 'https://schema.org',
    '@type': 'BlogPosting',
    headline: post.title,
    description: post.excerpt,
    image: `${baseUrl}${post.imageUrl}`,
    datePublished: post.publishedAt,
    author: {
      '@type': 'Person',
      name: post.author.name,
    },
  };

  return (
    <>
      <Title>{post.title} - My Blog</Title>

      {/* Basic Meta */}
      <Meta name="description" content={post.excerpt} />
      <Meta name="keywords" content={post.tags.join(', ')} />

      {/* Open Graph */}
      <Meta property="og:title" content={post.title} />
      <Meta property="og:description" content={post.excerpt} />
      <Meta property="og:image" content={`${baseUrl}${post.imageUrl}`} />
      <Meta property="og:url" content={canonicalUrl} />
      <Meta property="og:type" content="article" />

      {/* Twitter Card */}
      <Meta name="twitter:card" content="summary_large_image" />
      <Meta name="twitter:title" content={post.title} />
      <Meta name="twitter:description" content={post.excerpt} />
      <Meta name="twitter:image" content={`${baseUrl}${post.imageUrl}`} />

      {/* Canonical */}
      <link rel="canonical" href={canonicalUrl} />

      {/* Structured Data */}
      <Script type="application/ld+json">
        {JSON.stringify(structuredData)}
      </Script>

      <article>
        <h1>{post.title}</h1>
        <div>{post.content}</div>
      </article>
    </>
  );
}
```

## Layout-Level Document Management

Set document metadata in layouts for shared pages:

```tsx
// src/app/layout.tsx
import { Title, Meta } from '@putnami/web';

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <head>
        <Title>My App</Title>
        <Meta name="description" content="My awesome app" />
        <Meta property="og:site_name" content="My App" />
      </head>
      <body>
        {children}
      </body>
    </html>
  );
}
```

## Dynamic Meta Based on Route

```tsx
import { Meta } from '@putnami/web';
import { useLocation } from '@putnami/web';

export default function Page() {
  const location = useLocation();
  const baseUrl = 'https://example.com';

  return (
    <>
      <Meta property="og:url" content={`${baseUrl}${location.pathname}`} />
      {/* Other meta tags */}
    </>
  );
}
```

## Best Practices

1. **Set unique titles** - Each page should have a unique, descriptive title
2. **Write compelling descriptions** - Meta descriptions should be 150-160 characters
3. **Use Open Graph** - Essential for social media sharing
4. **Include canonical URLs** - Prevent duplicate content issues
5. **Add structured data** - Help search engines understand your content
6. **Optimize images** - Use proper image URLs for OG and Twitter cards
7. **Test your meta tags** - Use tools like Facebook Debugger and Twitter Card Validator

## Testing SEO

### Facebook Debugger
Test Open Graph tags: https://developers.facebook.com/tools/debug/

### Twitter Card Validator
Test Twitter Cards: https://cards-dev.twitter.com/validator

### Google Rich Results Test
Test structured data: https://search.google.com/test/rich-results

## Next Steps

- Learn about [Configuration](configuration.md) for custom HTML templates
- Explore [Advanced Patterns](advanced-patterns.md) for complex SEO scenarios
- Check [Troubleshooting](troubleshooting.md) for common document management issues
