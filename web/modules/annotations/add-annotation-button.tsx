'use client';

import { useState } from 'react';
import { Flag } from 'lucide-react';
import { Button } from '@/modules/shared/components/signal-primitives';
import { AnnotationDialog } from './annotation-dialog';
import type { AnnotationsController } from './use-annotations';

// AddAnnotationButton is the 44px affordance a temporal panel carries in its
// action row. It opens the shared AnnotationDialog against the panel's own
// window — the annotations the dialog lists are the ones the chart can show.
export function AddAnnotationButton({ annotations }: { annotations: AnnotationsController }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="outline" size="sm" className="min-h-[44px]" icon={<Flag size={14} />} onClick={() => setOpen(true)}>
        Add annotation
      </Button>
      {open ? <AnnotationDialog annotations={annotations} onClose={() => setOpen(false)} /> : null}
    </>
  );
}
