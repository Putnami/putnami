import { page } from '@putnami/web';
import { Badge, Card, CardBody, Heading, Link, Text, VStack } from '@putnami/ui';

const stack = [
  { name: 'Bun', desc: 'Fast JavaScript runtime and package manager' },
  { name: 'React', desc: 'Server-side rendered with client hydration' },
  { name: 'File-based routing', desc: 'Pages and API endpoints from the file system' },
  { name: 'Type-safe schemas', desc: 'Params, query, and body validation with inference' },
  { name: 'Fluent builders', desc: 'page(), layout(), loader(), action(), endpoint()' },
];

export default page().render(function AboutPage() {
  return (
    <VStack spacing='xl' style={{ maxWidth: '40rem' }}>
      <VStack as='header' spacing='xs'>
        <Heading level={1}>About this app</Heading>
        <Text color='secondary'>
          This application was scaffolded with{' '}
          <a href='https://putnami.dev' target='_blank' rel='noopener noreferrer'>
            Putnami
          </a>
          , a full-stack TypeScript framework for building web applications with Bun.
        </Text>
      </VStack>

      <VStack as='section' spacing='sm'>
        <Heading level={6} color='secondary'>
          The stack
        </Heading>
        {stack.map((item) => (
          <Card key={item.name} variant='outline'>
            <CardBody>
              <Badge colorScheme='primary' size='sm'>
                {item.name}
              </Badge>
              <Text color='secondary' size='sm'>
                {' '}
                {item.desc}
              </Text>
            </CardBody>
          </Card>
        ))}
      </VStack>

      <VStack as='section' spacing='sm'>
        <Heading level={6} color='secondary'>
          Project structure
        </Heading>
        <pre
          style={{
            background: 'var(--color-surface)',
            padding: 'var(--space-lg)',
            borderRadius: 'var(--radius-lg)',
            fontSize: '0.85rem',
            lineHeight: 1.6,
            overflow: 'auto',
          }}
        >
          {`src/
├── app/
│   ├── layout.tsx        Root layout
│   ├── page.tsx          Home page
│   ├── error.tsx         Error boundary
│   ├── not-found.tsx     404 page
│   ├── about/
│   │   └── page.tsx      This page
│   └── guestbook/
│       ├── page.tsx      Guestbook (Form + loader)
│       ├── loader.ts     Server data loading
│       └── action.ts     Server form handling
├── components/
│   └── counter.tsx       Client-side component
├── main.ts               App configuration
└── serve.ts              Entry point`}
        </pre>
      </VStack>

      <Link to='/'>Back to home</Link>
    </VStack>
  );
});
