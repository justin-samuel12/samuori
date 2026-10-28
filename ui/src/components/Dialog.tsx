import {
  Dialog as AriaDialog,
  Heading,
  Modal,
  ModalOverlay,
} from "react-aria-components";
import type { ReactNode } from "react";

/** confirm: a question with buttons; form: fields, in a side drawer; wide: editors, tables and long code. */
export type DialogSize = "confirm" | "form" | "wide";

interface Props {
  title: string;
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
  isDismissable?: boolean;
  size?: DialogSize;
  wide?: boolean;
}

export function Dialog({
  title,
  isOpen,
  onOpenChange,
  children,
  isDismissable = true,
  size,
  wide = false,
}: Props) {
  const kind = size ?? (wide ? "wide" : "form");
  return (
    <ModalOverlay
      className={`samuori-overlay samuori-overlay--${kind}`}
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      isDismissable={isDismissable}
    >
      <Modal className={`samuori-modal samuori-modal--${kind}`}>
        <AriaDialog className="samuori-dialog">
          <Heading slot="title">{title}</Heading>
          {children}
        </AriaDialog>
      </Modal>
    </ModalOverlay>
  );
}
