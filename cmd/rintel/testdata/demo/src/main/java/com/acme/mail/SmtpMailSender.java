package com.acme.mail;

import org.springframework.stereotype.Component;

@Component
public class SmtpMailSender implements MailSender {
    @Override
    public void send(String to, String body) {
        System.out.println(to + body);
    }
}
