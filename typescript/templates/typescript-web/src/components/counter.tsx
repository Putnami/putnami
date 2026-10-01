import { useState } from '@putnami/web';
import { Button, HStack, Text } from '@putnami/ui';

export function Counter() {
  const [count, setCount] = useState(0);

  return (
    <HStack spacing='sm' justify='center'>
      <Button variant='outline' size='sm' onClick={() => setCount((c: number) => c - 1)}>
        -
      </Button>
      <Text size='xl' weight='bold' style={{ minWidth: '3rem', textAlign: 'center' }}>
        {count}
      </Text>
      <Button variant='outline' size='sm' onClick={() => setCount((c: number) => c + 1)}>
        +
      </Button>
    </HStack>
  );
}
