'use client';

import { AnalysisPage } from '@/modules/analysis';
import { UTMLinkBuilder } from './link-builder';

export function AcquisitionPage() {
  return (
    <AnalysisPage boardKey="acquisition">
      <UTMLinkBuilder />
    </AnalysisPage>
  );
}
