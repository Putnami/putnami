import { Form, page, useLoaderData, useNavigation } from '@putnami/web';
import { Button, Card, CardBody, Heading, Input, Link, Text, VStack } from '@putnami/ui';
import type { GuestbookEntry } from './loader';

export default page().render(function GuestbookPage() {
  const { entries } = useLoaderData<{ entries: GuestbookEntry[] }>();
  const navigation = useNavigation();
  const isSubmitting = navigation.state === 'submitting';

  return (
    <VStack spacing='xl' style={{ maxWidth: '40rem' }}>
      <VStack as='header' spacing='xs'>
        <Heading level={1}>Guestbook</Heading>
        <Text color='secondary'>
          Leave a message. This demo uses a loader to fetch entries and an action to post new ones.
        </Text>
      </VStack>

      <Card variant='outline'>
        <CardBody>
          <Form method='post'>
            <VStack spacing='sm'>
              <Input name='name' placeholder='Your name' required fullWidth />
              <Input name='message' placeholder='Your message' required fullWidth />
              <Button type='submit' loading={isSubmitting} fullWidth>
                Post
              </Button>
            </VStack>
          </Form>
        </CardBody>
      </Card>

      <VStack as='section' spacing='sm'>
        <Heading level={6} color='secondary'>
          {entries.length} {entries.length === 1 ? 'entry' : 'entries'}
        </Heading>
        {entries.map((entry) => (
          <Card key={entry.id} variant='outline'>
            <CardBody>
              <VStack spacing='xs'>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  <Text weight='medium'>{entry.name}</Text>
                  <Text color='disabled' size='xs'>
                    {new Date(entry.createdAt).toLocaleDateString()}
                  </Text>
                </div>
                <Text color='secondary'>{entry.message}</Text>
              </VStack>
            </CardBody>
          </Card>
        ))}
      </VStack>

      <Link to='/'>Back to home</Link>
    </VStack>
  );
});
