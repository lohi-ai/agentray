import * as React from 'react';

type Direction = 'ltr' | 'rtl';
type Orientation = 'horizontal' | 'vertical';
type RootCommon = React.HTMLAttributes<HTMLDivElement> & {
  asChild?: boolean;
  disabled?: boolean;
  dir?: Direction;
  orientation?: Orientation;
};
type RootSingle = RootCommon & {
  type: 'single';
  value?: string;
  defaultValue?: string;
  onValueChange?: (value: string) => void;
  collapsible?: boolean;
};
type RootMultiple = RootCommon & {
  type: 'multiple';
  value?: string[];
  defaultValue?: string[];
  onValueChange?: (value: string[]) => void;
};
type ItemProps = React.HTMLAttributes<HTMLDivElement> & { value: string; disabled?: boolean; asChild?: boolean };
type HeaderProps = React.HTMLAttributes<HTMLHeadingElement> & { asChild?: boolean };
type TriggerProps = React.ButtonHTMLAttributes<HTMLButtonElement> & { asChild?: boolean };
type ContentProps = React.HTMLAttributes<HTMLDivElement> & { asChild?: boolean; forceMount?: true };

export declare const Root: React.ForwardRefExoticComponent<(RootSingle | RootMultiple) & React.RefAttributes<HTMLDivElement>>;
export declare const Item: React.ForwardRefExoticComponent<ItemProps & React.RefAttributes<HTMLDivElement>>;
export declare const Header: React.ForwardRefExoticComponent<HeaderProps & React.RefAttributes<HTMLHeadingElement>>;
export declare const Trigger: React.ForwardRefExoticComponent<TriggerProps & React.RefAttributes<HTMLButtonElement>>;
export declare const Content: React.ForwardRefExoticComponent<ContentProps & React.RefAttributes<HTMLDivElement>>;
export { Root as Accordion, Item as AccordionItem, Header as AccordionHeader, Trigger as AccordionTrigger, Content as AccordionContent };
