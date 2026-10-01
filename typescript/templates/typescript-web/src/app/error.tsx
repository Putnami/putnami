import { error } from '@putnami/web';
import { Heading, Text, VStack } from '@putnami/ui';

export default error().render(function ErrorPage() {
  return (
    <VStack spacing='sm' align='center' py='xxl'>
      <Heading level={1}>Something went wrong</Heading>
      <Text color='secondary'>Please try again later.</Text>
    </VStack>
  );
});
