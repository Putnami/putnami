import { page } from '@putnami/web';
import { Card, CardBody, Heading, HStack, Link, Text, VStack } from '@putnami/ui';

const docs = [
  {
    href: 'https://putnami.dev/docs/getting-started',
    label: 'Getting Started',
    desc: 'Set up your first project in minutes',
  },
  {
    href: 'https://putnami.dev/docs/how-to/build-a-web-app',
    label: 'Build a Web App',
    desc: 'React SSR with file-based routing',
  },
  {
    href: 'https://putnami.dev/docs/how-to/build-an-api-service',
    label: 'Build an API',
    desc: 'Type-safe endpoints with validation',
  },
  {
    href: 'https://putnami.dev/docs/frameworks/typescript/forms-and-actions',
    label: 'Forms & Actions',
    desc: 'Server-side form handling',
  },
  { href: 'https://putnami.dev/docs/tooling/cli', label: 'CLI Reference', desc: 'Workspace commands and tooling' },
];

export default page().render(function HomePage() {
  return (
    <VStack spacing='xl' style={{ maxWidth: '40rem' }}>
      <VStack as='header' spacing='xs' align='center'>
        <Heading level={1} align='center'>
          Welcome to Putnami
        </Heading>
        <Text color='secondary' size='lg'>
          Your React application is up and running.
        </Text>
      </VStack>

      <VStack as='nav' spacing='sm'>
        <Heading level={6} color='secondary'>
          Explore this app
        </Heading>
        <HStack spacing='sm'>
          <Card variant='outline' p='md'>
            <Link to='/about'>About</Link>
          </Card>
          <Card variant='outline' p='md'>
            <Link to='/guestbook'>Guestbook</Link>
          </Card>
        </HStack>
      </VStack>

      <VStack as='section' spacing='sm'>
        <Heading level={6} color='secondary'>
          Documentation
        </Heading>
        {docs.map((doc) => (
          <Card key={doc.href} variant='outline'>
            <CardBody>
              <a
                href={doc.href}
                target='_blank'
                rel='noopener noreferrer'
                style={{ textDecoration: 'none', color: 'inherit' }}
              >
                <Text weight='medium'>{doc.label}</Text>
                <Text color='secondary' size='sm'>
                  {' '}
                  {doc.desc}
                </Text>
              </a>
            </CardBody>
          </Card>
        ))}
      </VStack>
    </VStack>
  );
});
