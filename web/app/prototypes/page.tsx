import { permanentRedirect } from 'next/navigation';

// /prototypes is the pre-Plans name for the same validation_tests list. The
// route stays only as a permanent redirect so agent notes and saved deep
// links keep resolving; the modules/prototypes code remains on disk as the
// design artifact /plans still reads from.
export default function PrototypesRoute() {
  permanentRedirect('/plans');
}
