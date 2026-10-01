import { loader } from '@putnami/web';
import { tasks } from '../../store';

export default loader().handle(async () => ({ tasks: [...tasks.values()] }));
