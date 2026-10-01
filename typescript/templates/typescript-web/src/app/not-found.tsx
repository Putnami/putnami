import { notFound } from '@putnami/web';
import { Heading, Text, VStack } from '@putnami/ui';

export default notFound().render(function NotFoundPage() {
  return (
    <VStack spacing='sm' align='center' py='xxl'>
      <Heading level={1} size='4xl' color='disabled'>
        404
      </Heading>
      <Text color='secondary' size='lg'>
        Page not found
      </Text>
    </VStack>
  );
});
